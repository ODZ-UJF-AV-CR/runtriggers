package main

import (
	"encoding/base64"
	"net/http"
	"strings"
	"time"
)

/*
Code below taken from gddo/gddo-server/template.go.

Copyright (c) 2013 The Go Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
*/

type flashMessage struct {
	ID   string
	Args []string
}

// getFlashMessages retrieves flash messages from the request and clears the flash cookie if needed.
func getFlashMessages(resp http.ResponseWriter, req *http.Request) []flashMessage {
	c, err := req.Cookie("flash")
	if err == http.ErrNoCookie {
		return nil
	}
	http.SetCookie(resp, &http.Cookie{Name: "flash", Path: "/", MaxAge: -1, Expires: time.Now().Add(-100 * 24 * time.Hour)})
	if err != nil {
		return nil
	}
	p, err := base64.URLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	var messages []flashMessage
	for _, s := range strings.Split(string(p), "\000") {
		idArgs := strings.Split(s, "\001")
		messages = append(messages, flashMessage{ID: idArgs[0], Args: idArgs[1:]})
	}
	return messages
}

// setFlashMessages sets a cookie with the given flash messages.
func setFlashMessages(resp http.ResponseWriter, messages []flashMessage) {
	var buf []byte
	for i, message := range messages {
		if i > 0 {
			buf = append(buf, '\000')
		}
		buf = append(buf, message.ID...)
		for _, arg := range message.Args {
			buf = append(buf, '\001')
			buf = append(buf, arg...)
		}
	}
	value := base64.URLEncoding.EncodeToString(buf)
	http.SetCookie(resp, &http.Cookie{Name: "flash", Value: value, Path: "/"})
}
