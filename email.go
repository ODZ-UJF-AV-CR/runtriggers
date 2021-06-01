package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"text/template"
)

var (
	flagEmailProgram = flag.String("email", "", "name of a program through which to send out email")
	flagBacklink     = flag.String("backlink", "", "base link to this instance to use in email texts (without the path prefix)")
)

func backLink(p string) string {
	base, _ := url.Parse(*flagBacklink)
	base.Path = path.Join("/", *flagBasePath, p)
	return base.String()
}

const tmplAnomalous = `Content-Type: text/plain; charset=utf-8
Subject: =?utf-8?B?{{ .script.Name | printf "%q anomalous" | base64 }}?=

Hello!

Run #{{ .script.RunCounter }} of your script {{ .script.Name | printf "%q" }} exited with non-zero exit code.

Run time: {{ .run.Duration | FormatDuration }}
Log: {{ printf "/scripts/%d/logs/%d" .script.ID .script.RunCounter | backlink }}
Script: {{ printf "/scripts/%d" .script.ID | backlink }}

This is an automatic email.
`

const tmplInterrupted = `Content-Type: text/plain; charset=utf-8
Subject: =?utf-8?B?{{ .script.Name | printf "%q interrupted" | base64 }}?=

Hello!

Run #{{ .script.RunCounter }} of your script {{ .script.Name | printf "%q" }} has been interrupted.

Script: {{ printf "/scripts/%d" .script.ID | backlink }}

This is an automatic email.
`

var emailFuncs template.FuncMap = template.FuncMap{
	"sh":             Sh,
	"backlink":       backLink,
	"FormatDuration": FormatDuration,
	"base64":         func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) },
}

func notifyAnomalous(s Script, r Run) {
	t, err := template.New("").Funcs(emailFuncs).Parse(tmplAnomalous)
	if err != nil {
		log.Printf("email notify: failed to parse template: %s", err)
		return
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]interface{}{
		"script": s,
		"run":    r,
	})
	if err != nil {
		log.Printf("email notify: failed to execute template: %s", err)
		return
	}

	log.Printf("email notify: sending to %q", s.EmailAddress)

	cmd := exec.Command(*flagEmailProgram, s.EmailAddress)
	cmd.Stdin = &b
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("email notify: %s", err)
	}
}

func notifyInterrupted(s Script) {
	t, err := template.New("").Funcs(emailFuncs).Parse(tmplInterrupted)
	if err != nil {
		log.Printf("email notify: failed to parse template: %s", err)
		return
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]interface{}{
		"script": s,
	})
	if err != nil {
		log.Printf("email notify: failed to execute template: %s", err)
		return
	}

	log.Printf("email notify: sending to %q", s.EmailAddress)

	cmd := exec.Command(*flagEmailProgram, s.EmailAddress)
	cmd.Stdin = &b
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	err = cmd.Run()
	if err != nil {
		log.Printf("email notify: %s", err)
	}
}
