package main

import (
	"bytes"
	"database/sql/driver"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
)

type Script struct {
	ID         int `gorm:"primary_key"`
	Owner      user
	Text       string
	RunCounter int

	Name                        string `param:"string"`
	RunPeriod                   string `param:"string"`
	PeriodicRunsEnabled         bool   `param:"bool"`
	ScheduledRunsEnabled        bool   `param:"bool"`
	AutomaticRunsDisableOnError bool   `param:"bool"`

	EmailNotification bool   `param:"bool"`
	EmailAddress      string `param:"string"`

	Scheduled *time.Time

	started       bool           `gorm:"-"`
	stopch        chan struct{}  `gorm:"-"`
	manualch      chan struct{}  `gorm:"-"`
	quitch        chan struct{}  `gorm:"-"`
	killch        chan os.Signal `gorm:"-"`
	updateschedch chan struct{}  `gorm:"-"`

	changechM sync.Mutex
	changech  chan struct{} `gorm:"-"`
}

func (s *Script) Copy() Script {
	var copy Script
	copy = *s

	copy.started = false
	copy.stopch = nil
	copy.manualch = nil
	copy.quitch = nil
	copy.killch = nil
	copy.updateschedch = nil

	return copy
}

type Cause int

const (
	CauseUnknown Cause = iota
	CauseManual
	CauseScheduled
	CausePeriodic

/*	CauseFilesystem
	CauseExternal */
)

func (c Cause) String() string {
	switch c {
	case CauseUnknown:
		return "unknown"
	case CauseManual:
		return "manual"
	case CauseScheduled:
		return "scheduled"
	case CausePeriodic:
		return "periodic"
		/*
			case CauseFilesystem:
				return "filesystem"
			case CauseExternal:
				return "external" */
	default:
		return "<invalid cause>"
	}
}

func (c Cause) Value() driver.Value {
	return driver.Value(int64(c))
}

func (c *Cause) Scan(src interface{}) error {
	switch m := src.(type) {
	case int64:
		*c = Cause(m)
		return nil
	default:
		return errors.New("type mismatch")
	}
}

type State int

const (
	StateRunning State = iota
	StateFailed
	StateInterrupted
	StateKilled
	StateDone
	StateNonzeroCode
)

func (s State) String() string {
	switch s {
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	case StateInterrupted:
		return "interrupted"
	case StateKilled:
		return "killed"
	case StateDone:
		return ""
	case StateNonzeroCode:
		return "non-zero code"
	default:
		return "<invalid state>"
	}
}

func (s State) Running() bool {
	return s == StateRunning
}

func (s State) Value() driver.Value {
	return driver.Value(int64(s))
}

func (s *State) Scan(src interface{}) error {
	switch m := src.(type) {
	case int64:
		*s = State(m)
		return nil
	default:
		return errors.New("type mismatch")
	}
}

type Run struct {
	Scheduled *time.Time

	StartTime  time.Time
	FinishTime *time.Time
	ExitCode   int
	State

	LogFilename string

	Cause Cause

	ScriptID int `gorm:"primary_key;auto_increment:false"`
	RunNo    int `gorm:"primary_key;auto_increment:false"`
	Script   *Script
}

func (r Run) Duration() time.Duration {
	if r.FinishTime != nil {
		return r.FinishTime.Sub(r.StartTime)
	} else {
		return 0
	}
}

func (s *Script) loop() {
loop:
	for {
		var cause Cause
	wait:
		for {
			if err := db.First(s, s.ID).Error; err != nil {
				log.Printf("failed to re-read script: %s", err)
			}

			var schedulech <-chan time.Time
			var periodch <-chan time.Time

			if s.ScheduledRunsEnabled && s.Scheduled != nil {
				duration := s.Scheduled.Sub(time.Now())
				if duration < 0 {
					duration = 0
				}
				schedulech = time.After(duration)
			}

			if s.PeriodicRunsEnabled {
				var err error
				var period time.Duration
				var lastRunTime time.Time
				if period, err = time.ParseDuration(s.RunPeriod); err != nil {
					log.Printf("%q: failed to parse period %q", s.Name, s.RunPeriod)
					period = 356 * 24 * time.Hour
				}
				if s.RunCounter == 0 {
					period = 0 /* no last run, cause an immediate run */
				} else {
					var lastRun Run
					if err = db.Where("script_id=? AND run_no=?", s.ID, s.RunCounter).First(&lastRun).Error; err == nil {
						lastRunTime = lastRun.StartTime
					} else {
						log.Printf("failed to find last run: %s", err.Error())
						lastRunTime = time.Now()
					}
				}
				periodch = time.After(lastRunTime.Add(period).Sub(time.Now()))
			}

			select {
			case <-s.updateschedch:
				/* no-op */
			case <-s.stopch:
				break loop
			case <-s.manualch:
				cause = CauseManual
				break wait
			case <-schedulech:
				cause = CauseScheduled
				break wait
			case <-periodch:
				cause = CausePeriodic
				break wait
			}
		}

		s.run(cause)
	}

	close(s.quitch)
}

func parseShebang(code string) []string {
	if !(len(code) >= 2 && code[0:2] == "#!") {
		return nil
	}

	eol := strings.IndexByte(code[2:], '\n')
	if eol == -1 {
		return nil
	}
	eol += 2

	line := strings.TrimSpace(code[2:eol])
	sep := strings.IndexByte(line, ' ')
	if sep == -1 {
		return []string{line}
	} else {
		return []string{line[:sep], line[sep+1:]}
	}
}

func logFilename(run Run) string {
	return filepath.Join(*flagLogDir, fmt.Sprintf("%04d/%s.log", run.Script.ID, run.StartTime.Format("060102/15040507")))
}

func (s *Script) schedule(nextRun time.Time) {
	if err := db.Model(s).Update("Scheduled", nextRun).Error; err != nil {
		log.Printf("failed to reschedule: %s", err)
	}
	// signal to the script's loop to re-read the script from DB
	select {
	case s.updateschedch <- struct{}{}:
	default:
	}
}

func (s *Script) unschedule() {
	if err := db.Model(s).Update("Scheduled", nil).Error; err != nil {
		log.Printf("failed to reschedule: %s", err)
	}
	select {
	case s.updateschedch <- struct{}{}:
	default:
	}
}

func (s *Script) run(cause Cause) {
	var run Run
	var err error
	run.StartTime = time.Now()
	run.Script = s
	run.Cause = cause
	s.RunCounter += 1
	run.RunNo = s.RunCounter
	run.LogFilename = logFilename(run)
	run.State = StateRunning

	// update the script's run counter first
	if err = db.Save(s).Error; err != nil {
		log.Printf("failed to save script: %s", err)
		return
	}

	if err = db.Create(&run).Error; err != nil {
		log.Printf("failed to create run: %s", err)
		return
	}

	s.broadcastChange()
	defer s.broadcastChange()

	if cause == CauseScheduled {
		// clear the scheduled time
		s.Scheduled = nil
		if err = db.Save(s).Error; err != nil {
			log.Printf("failed to save script: %s", err)
			return
		}
	}

	defer func() {
		now := time.Now()
		run.FinishTime = &now
		if run.State == StateRunning {
			run.State = StateDone
		}

		scriptChange := false

		if run.State != StateDone && s.EmailNotification {
			go notifyAnomalous(*s, run)
			s.EmailNotification = false
			scriptChange = true
		}

		if run.State != StateDone && s.AutomaticRunsDisableOnError {
			s.ScheduledRunsEnabled = false
			s.PeriodicRunsEnabled = false
			scriptChange = true
		}

		if scriptChange {
			if err = db.Save(s).Error; err != nil {
				log.Printf("failed to save script: %s", err)
			}
		}

		if err = db.Save(&run).Error; err != nil {
			log.Printf("failed to save run: %s", err)
		}
	}()

	os.MkdirAll(filepath.Dir(run.LogFilename), 0755)
	f, err := os.Create(run.LogFilename)
	if err != nil {
		log.Printf("failed to create log file: %s", err)
		run.State = StateFailed
		return
	}
	defer f.Close()

	argv := parseShebang(s.Text)
	if argv == nil {
		argv = []string{"/bin/sh"}
	}

	suExec, err := exec.LookPath("su-exec")
	if err != nil {
		fmt.Fprintf(f, "runtriggers: %s\n", err)
		run.State = StateFailed
		return
	}
	trueArgv := append([]string{suExec, string(s.Owner)}, argv...)
	cmd := exec.Cmd{
		Path:   trueArgv[0],
		Args:   trueArgv,
		Stdin:  bytes.NewBufferString(s.Text),
		Stderr: f,
		Stdout: f,
	}

	if err = cmd.Start(); err != nil {
		fmt.Fprintf(f, "runtriggers: process run failed: %s\n", err)
		run.State = StateFailed
		return
	}

	waitch := make(chan struct{})
	go func() {
		cmd.Wait()
		close(waitch)
	}()

waitloop:
	for {
		select {
		case signal := <-s.killch:
			cmd.Process.Signal(signal)
			run.State = StateKilled
			fmt.Fprintf(f, "runtriggers: sending signal %d\n", signal)
		case <-waitch:
			break waitloop
		}
	}

	code := cmd.ProcessState.ExitCode()
	run.ExitCode = code
	if code != 0 && run.State == StateRunning {
		run.State = StateNonzeroCode
	}
	fmt.Fprintf(f, "runtriggers: '%s' exited with code %d\n", argv[0], code)
}

func (s *Script) broadcastChange() {
	s.changechM.Lock()
	close(s.changech)
	s.changech = make(chan struct{})
	s.changechM.Unlock()
}

func (s *Script) getChangech() chan struct{} {
	s.changechM.Lock()
	defer s.changechM.Unlock()
	return s.changech
}

func (s *Script) start() {
	if s.started {
		log.Printf("BUG: script started twice")
		return
	}

	s.started = true
	s.stopch = make(chan struct{})
	s.quitch = make(chan struct{})
	s.killch = make(chan os.Signal)
	s.manualch = make(chan struct{}, 1)
	s.updateschedch = make(chan struct{}, 1)
	s.changech = make(chan struct{})

	go s.loop()
}

func (s *Script) stop() error {
	select {
	case s.stopch <- struct{}{}:
	default:
		return errors.New("script busy")
	}

	<-s.quitch

	return nil
}

func (s *Script) kill(sig os.Signal) error {
	select {
	case s.killch <- sig:
	default:
		return errors.New("script not running")
	}

	return nil
}

func (s *Script) manual() error {
	select {
	case s.manualch <- struct{}{}:
		return nil
	default:
		return errors.New("script busy")
	}
}

var db *gorm.DB

type scriptList struct {
	scripts map[int]*Script
	sync.RWMutex
}

var allScripts scriptList = scriptList{
	scripts: make(map[int]*Script),
}

func (list scriptList) get() []*Script {
	list.RLock()
	defer list.RUnlock()

	var ret []*Script
	for _, v := range list.scripts {
		ret = append(ret, v)
	}

	return ret
}

func (list scriptList) save(script *Script) error {
	list.Lock()
	defer list.Unlock()

	if oldScript, ok := list.scripts[script.ID]; !ok {
		if err := db.Create(script).Error; err != nil {
			return err
		}
	} else {
		if err := oldScript.stop(); err != nil {
			return err
		}

		if err := db.Save(script).Error; err != nil {
			return err
		}
	}

	list.scripts[script.ID] = script
	script.start()

	return nil
}

func (list scriptList) lookup(id int) (*Script, bool) {
	list.RLock()
	defer list.RUnlock()
	script, ok := list.scripts[id]
	return script, ok
}

func (list scriptList) delete(id int) error {
	list.RLock()
	defer list.RUnlock()
	script, ok := list.scripts[id]
	if !ok {
		return nil
	}
	if err := script.stop(); err != nil {
		return err
	}
	if err := db.Delete(script).Error; err != nil {
		return err
	}
	delete(list.scripts, id)
	return nil
}

func initDatabase() {
	var err error
	db, err = gorm.Open("sqlite3", *flagDatabase)
	if err != nil {
		panic("failed to connect database")
	}

	db.AutoMigrate(&Script{})
	db.AutoMigrate(&Run{})

	db.Exec(
		"UPDATE scripts SET scheduled_runs_enabled=false, periodic_runs_enabled=false "+
			"WHERE id IN ("+
			"SELECT id FROM scripts LEFT JOIN runs WHERE runs.script_id=scripts.id "+
			"AND scripts.run_counter=runs.run_no AND runs.state=? "+
			"AND scripts.automatic_runs_disable_on_error "+
			")",
		int64(StateRunning),
	)

	var interrupted []Run
	if err := db.Where("state=?", int64(StateRunning)).Find(&interrupted).Error; err != nil {
		log.Printf("failed to list stale runs: %s", err)
	}
	for _, run := range interrupted {
		run.Script = &Script{}
		if err := db.Where("id=?", run.ScriptID).First(run.Script).Error; err != nil {
			log.Printf("failed to load script: %s", err)
		}
		if run.Script.EmailNotification {
			run.Script.EmailNotification = false
			if err := db.Save(run.Script).Error; err != nil {
				log.Printf("failed to save script: %s", err)
			} else {
				go notifyInterrupted(*run.Script)
			}
		}
	}

	db.Exec(
		"UPDATE runs SET state=? WHERE state=?",
		int64(StateInterrupted), int64(StateRunning),
	)

	var scripts []Script
	if err := db.Find(&scripts).Error; err != nil {
		log.Fatalf("failed to load scripts: %s", err)
	}

	for i, _ := range scripts {
		script := scripts[i]
		allScripts.scripts[script.ID] = &script
		script.start()
	}
}
