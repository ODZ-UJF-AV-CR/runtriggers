package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/hpcloud/tail"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type stateMessage struct {
	Type    string `json:"t"`
	Running bool   `json:"running"`
	Exited  bool   `json:"exited"`
	Code    int    `json:"code"`
	Log     string `json:"-"`
}

func fetchScriptState(s *Script) (stateMessage, error) {
	var runs []Run
	db.Where("script_id = ?", s.ID).Order("start_time desc").Limit(1).Find(&runs)

	var ret stateMessage
	ret.Type = "state"

	if len(runs) >= 1 {
		ret.Running = (runs[0].State == StateRunning)
		ret.Exited = !ret.Running
		ret.Code = runs[0].ExitCode
		ret.Log = runs[0].LogFilename
	}

	return ret, nil
}

func logWstail(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	id, _ := strconv.Atoi(vars["id"])
	s, ok := allScripts.lookup(id)

	if !ok {
		http.NotFound(w, r)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("log tail: %s: failed to upgrade: %s\n", r.RemoteAddr, err)
		return
	}

	var state, newState stateMessage
	var changech chan struct{}
	var lines chan *tail.Line
	var tailObj *tail.Tail

	{
		// set changech to closed channel to invoke one initial state fetching
		changech = make(chan struct{})
		close(changech)
	}

	readErr := make(chan error, 1)
	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				break
			}
		}
	}()

	for {
		select {
		case <-changech:
			changech = s.getChangech()
			newState, err = fetchScriptState(s)
			if err != nil {
				goto err
			}
			if newState != state {
				err = conn.WriteJSON(newState)
				if err != nil {
					goto err2
				}
			}
			state = newState
			if (tailObj == nil || state.Log != tailObj.Filename) && state.Log != "" {
				err = conn.WriteJSON(struct {
					Type string `json:"t"`
				}{
					Type: "flush",
				})
				if err != nil {
					goto err
				}

				if tailObj != nil {
					tailObj.Stop()
				}

				tailObj, err = tail.TailFile(
					state.Log,
					tail.Config{Follow: true, Location: &tail.SeekInfo{0, os.SEEK_SET}},
				)
				if err != nil {
					goto err
				}
				lines = tailObj.Lines
			}

		case line := <-lines:
			conn.WriteJSON(struct {
				Type string `json:"t"`
				Line string `json:"line"`
			}{
				Type: "logline",
				Line: line.Text,
			})

		case err = <-readErr:
			goto err2
		}
	}

err:
	if err != nil {
		conn.WriteJSON(struct {
			Type    string `json:"t"`
			Message string `json:"msg"`
		}{
			Type:    "error",
			Message: err.Error(),
		})
	}

err2:
	if tailObj != nil {
		tailObj.Stop()
	}
	conn.Close()

	if err != nil {
		log.Printf("request from %s, script %d: log tailing: %s", r.RemoteAddr, id, err)
	}
}
