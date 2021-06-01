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

func logWstail(w http.ResponseWriter, r *http.Request, u user) {
	vars := mux.Vars(r)
	scriptId, _ := strconv.Atoi(vars["id"])
	runNo, _ := strconv.Atoi(vars["runno"])

	var run Run
	err := db.Where("script_id = ?", scriptId).Where("run_no = ?", runNo).First(&run).Error
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	filename := run.LogFilename
	config := tail.Config{Follow: true}
	config.Location = &tail.SeekInfo{0, os.SEEK_SET}
	t, err := tail.TailFile(filename, config)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("log tail: %s: failed to upgrade: %s\n", r.RemoteAddr, err)
		return
	}

	for line := range t.Lines {
		conn.WriteMessage(websocket.TextMessage, []byte(line.Text))
	}

	t.Wait()
}
