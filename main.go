package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	osUser "os/user"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/mux"
)

var (
	errForbidden = errors.New("forbidden")
)

var (
	flagDebug      = flag.Bool("debug", false, "run in debug mode")
	flagListenAddr = flag.String("listen", "127.0.0.1:80", "address for HTTP server to listen on")
	flagDatabase   = flag.String("db", "test.db", "path to sqlite database")
	flagLogDir     = flag.String("log", "/tmp/runtriggers_logs", "path to directory to store logs in")

	flagBasePath = flag.String("basepath", "", "path prefix of runtriggers web tree")
	flagMockUser = flag.String("mockuser", "", "")
)

func Link(p string) string {
	return path.Join("/", *flagBasePath, p)
}

func mustOpen(name string) *os.File {
	f, err := os.Open(name)
	if err != nil {
		panic(err)
	}
	return f
}

var (
	devNull = mustOpen(os.DevNull)
)

var templatesPath string
var staticPath string

func initPaths() {
	dataDir := os.Getenv("RUNTRIGGERS_DATA_DIR")
	if dataDir == "" {
		execPath, err := os.Executable()
		if err != nil {
			log.Panicf("os.Executable(): %s", err)
		}
		dataDir = path.Dir(execPath)
	}

	templatesPath = path.Join(dataDir, "templates")
	staticPath = path.Join(dataDir, "static")
}

var (
	templates atomic.Value /* *template.Template */
)

func Sh(shCmd string) string {
	cmd := exec.Command("/bin/sh", "-c", shCmd)
	cmd.Stdin = devNull
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return fmt.Sprintf("running '%s' failed: %v", shCmd, err.Error())
	}
	return out.String()
}

func readTemplates() (*template.Template, error) {
	funcMap := template.FuncMap{
		"sh":             Sh,
		"link":           Link,
		"FormatDuration": FormatDuration,
	}

	return template.New("").Funcs(funcMap).ParseGlob(templatesPath + "/*")
}

func loadTemplates() {
	new, err := readTemplates()
	if err != nil {
		log.Printf("error reading templates: %s", err)
		return
	}
	templates.Store(new)
}

func initTemplates() {
	loadTemplates()
	if templates.Load() == nil {
		log.Fatalf("failed to read templates")
	}
}

func tmpl() *template.Template {
	if *flagDebug {
		loadTemplates()
	}

	return templates.Load().(*template.Template)
}

func execTmpl(w http.ResponseWriter, name string, data interface{}) {
	funcMap := template.FuncMap{
		"sh":             Sh,
		"link":           Link,
		"FormatDuration": FormatDuration,
	}

	templ, err := template.New("").Funcs(funcMap).ParseFiles(
		filepath.Join(templatesPath, "shared.html"),
		filepath.Join(templatesPath, name+".html"),
	)
	if err != nil {
		if *flagDebug {
			http.Error(w, err.Error(), 500)
		} else {
			http.Error(w, "internal server error", 500)
		}
		log.Printf("executing template %s: %s\n", name, err)
	}
	templ.ExecuteTemplate(w, name+".html", data)
	/*
		err := tmpl().ExecuteTemplate(w, name, data)
		if err != nil {
			if *flagDebug {
				http.Error(w, err.Error(), 500)
			} else {
				http.Error(w, "internal server error", 500)
			}
			log.Printf("executing template %s: %s\n", name, err)
		}
	*/
}

func fillScripts(runs []Run) {
	for i := range runs {
		runs[i].Script, _ = allScripts.lookup(runs[i].ScriptID)
	}
}

func setFlashAndRedirect(w http.ResponseWriter, r *http.Request, url string, typ, msg string) {
	setFlashMessages(w, []flashMessage{{ID: typ, Args: []string{msg}}})
	http.Redirect(w, r, url, http.StatusFound)
}

func listJobs(w http.ResponseWriter, r *http.Request, u user) {
	flashMessages := getFlashMessages(w, r)

	var runs []Run
	db.Order("start_time desc").Limit(25).Find(&runs)
	fillScripts(runs)

	execTmpl(w, "list", map[string]interface{}{
		"user":          u,
		"flashMessages": flashMessages,
		"scripts":       allScripts.get(),
		"runs":          runs,
	})
}

func getRequestUser(r *http.Request) user {
	if *flagMockUser != "" {
		return user(*flagMockUser)
	} else {
		return user(r.Header.Get("X-Forwarded-User"))
	}
}

func requireLogin(handler func(http.ResponseWriter, *http.Request, user)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		user := getRequestUser(r)

		if user == "" {
			http.Error(w, "forbidden", 403)
		} else {
			handler(w, r, user)
		}
	}
}

func updateScriptFromForm(w http.ResponseWriter, r *http.Request, s *Script, u user) {
	flashMessages := getFlashMessages(w, r)
	issues := make(map[string]string)

	r.ParseForm()
	s.Text = strings.ReplaceAll(r.Form.Get("Text"), "\r\n", "\n")

	v := reflect.ValueOf(s).Elem()
	t := reflect.TypeOf(*s)
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		tag := t.Field(i).Tag.Get("param")
		switch tag {
		case "bool":
			v.Field(i).SetBool(r.Form.Get(name) == "on")
		case "string":
			v.Field(i).SetString(r.Form.Get(name))
		case "":
		default:
			log.Printf("field '%s' of Script has an unhandled param tag with value %s!",
				name, tag)
		}
	}

	if s.Name == "" {
		issues["Name"] = "Name cannot be empty"
	}

	if _, err := time.ParseDuration(s.RunPeriod); s.PeriodicRunsEnabled && err != nil {
		issues["RunPeriod"] = "Period invalid: " + err.Error()
	}

	if len(issues) == 0 {
		new := s.ID == 0

		if err := allScripts.save(s); err != nil {
			flashMessages = append(flashMessages, flashMessage{
				ID:   "error",
				Args: []string{fmt.Sprintf("Failed to save script: %s", err)},
			})
		} else {
			var msg string
			if new {
				msg = "Script created"
			} else {
				if r.Form.Get("save_and_run") == "1" {
					s.manual()
					msg = "Script updated & manually triggered to run"
				} else {
					msg = "Script updated"
				}
			}
			setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "success", msg)
		}
	}

	execTmpl(w, "script", map[string]interface{}{
		"user":          u,
		"flashMessages": flashMessages,
		"Script":        s,
		"issues":        issues,
	})
}

func newScript(w http.ResponseWriter, r *http.Request, u user) {
	s := Script{Owner: u}

	if r.Method == "POST" {
		updateScriptFromForm(w, r, &s, u)
	} else {
		execTmpl(w, "script", map[string]interface{}{
			"user":          u,
			"flashMessages": getFlashMessages(w, r),
			"Script":        s,
			"issues":        map[string]string{},
		})
	}
}

func showScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])

	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	var runs []Run
	db.Where("script_id = ?", s.ID).Order("start_time desc").Limit(10).Find(&runs)

	var running bool
	if len(runs) >= 1 {
		running = (runs[0].State == StateRunning)
	}

	if r.Method == "POST" {
		newScript := s.Copy()
		updateScriptFromForm(w, r, &newScript, u)
	} else {
		execTmpl(w, "script", map[string]interface{}{
			"user":          u,
			"flashMessages": getFlashMessages(w, r),
			"Script":        s,
			"issues":        map[string]string{},
			"runs":          runs,
			"running":       running,
		})
	}
}

func runScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	s.manual()

	setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "info", "Triggered a manual run")
}

func deleteScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	err := allScripts.delete(id)
	if err != nil {
		setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "error", fmt.Sprintf("Failed to delete script: %s", err))
	} else {
		setFlashAndRedirect(w, r, Link("/"), "success", fmt.Sprintf("Script '%s' deleted", s.Name))
	}
}

func scheduleScriptX(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := ioutil.ReadAll(r.Body)

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	time, err := parseTime(string(body))

	if err != nil {
		http.Error(w, fmt.Sprintf("parsing body: %s", err), 500)
		return
	}

	s.schedule(time)
}

func scheduleScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	r.ParseForm()
	time, err := parseTime(string(r.Form.Get("Time")))

	if err != nil {
		setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "error", fmt.Sprintf("Could not schedule: %s", err))
		return
	}

	s.schedule(time)
	setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "success", "Script scheduled")
}

func unscheduleScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	s.unschedule()
	setFlashAndRedirect(w, r, Link(fmt.Sprintf("/scripts/%d", s.ID)), "success", "Script scheduling cleared")
}

func killScript(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	signo, _ := strconv.Atoi(vars["signo"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	var err error

	sig := syscall.Signal(signo)
	switch sig {
	case syscall.SIGKILL:
	case syscall.SIGTERM:
	case syscall.SIGINT:
	default:
		err = errors.New("bad signal no")
	}

	if err == nil {
		err = s.kill(sig)
	}

	redirURL := Link(fmt.Sprintf("/scripts/%d", s.ID))
	if err != nil {
		setFlashAndRedirect(w, r, redirURL, "error", fmt.Sprintf("Could not send signal: %s", err))
		return
	}
	setFlashAndRedirect(w, r, redirURL, "success", "Signal sent")
}

func viewLog(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	scriptId, _ := strconv.Atoi(vars["id"])
	runNo, _ := strconv.Atoi(vars["runno"])

	var run Run
	err := db.Debug().Where("script_id = ? AND run_no = ?", scriptId, runNo).First(&run).Error
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	log.Printf("%r", run)

	http.ServeFile(w, r, run.LogFilename)
}

func manual(w http.ResponseWriter, r *http.Request, u user) {
	flashMessages := getFlashMessages(w, r)

	execTmpl(w, "manual", map[string]interface{}{
		"user":          u,
		"flashMessages": flashMessages,
	})
}

func main() {
	flag.Parse()

	initPaths()
	initTemplates()
	initUsers()
	initDatabase()

	r := mux.NewRouter()

	// TODO: limit to POST
	r.HandleFunc("/scripts", requireLogin(newScript))
	r.HandleFunc("/scripts/new", requireLogin(newScript))
	r.HandleFunc("/scripts/{id:[0-9]+}", requireLogin(showScript))
	r.HandleFunc("/scripts/{id:[0-9]+}/run", requireLogin(runScript))
	r.HandleFunc("/scripts/{id:[0-9]+}/delete", requireLogin(deleteScript))
	r.HandleFunc("/scripts/{id:[0-9]+}/x-schedule", scheduleScriptX)                   // TODO: put only
	r.HandleFunc("/scripts/{id:[0-9]+}/schedule", requireLogin(scheduleScript))        // TODO: put only
	r.HandleFunc("/scripts/{id:[0-9]+}/unschedule", requireLogin(unscheduleScript))    // TODO: put only
	r.HandleFunc("/scripts/{id:[0-9]+}/kill/{signo:[0-9]+}", requireLogin(killScript)) // TODO: put only
	r.HandleFunc("/scripts/{id:[0-9]+}/logs/{runno:[0-9]+}", requireLogin(viewLog))
	r.HandleFunc("/scripts/{id:[0-9]+}/logs/{runno:[0-9]+}/wstail", requireLogin(logWstail))

	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(staticPath))))
	r.HandleFunc("/", requireLogin(listJobs))
	r.HandleFunc("/manual", requireLogin(manual))

	h := http.StripPrefix(*flagBasePath, r)

	var l net.Listener
	var err error
	addr := *flagListenAddr
	if strings.HasPrefix(addr, "unix:") {
		path := strings.TrimPrefix(addr, "unix:")
		if _, err = os.Stat(path); err == nil {
			syscall.Unlink(path)
		}
		l, err = net.Listen("unix", strings.TrimPrefix(addr, "unix:"))
		if err != nil {
			log.Fatal(err)
		}
		if err = os.Chmod(path, 0775); err != nil {
			log.Printf("failed changing socket file permissions mode: %s", err)
		}
		if grp, err := osUser.LookupGroup("runtriggers"); err == nil {
			gid, _ := strconv.Atoi(grp.Gid)
			if err = os.Chown(path, os.Getuid(), gid); err != nil {
				log.Printf("failed changing socket file group ownership: %s", err)
			}
		}
	} else {
		l, err = net.Listen("tcp", addr)
	}
	if err != nil {
		log.Fatal(err)
	}

	err = http.Serve(l, h)
	if err != nil {
		log.Fatal(err)
	}
}
