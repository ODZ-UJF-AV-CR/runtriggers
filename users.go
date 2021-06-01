package main

import (
	"flag"
	"strings"
)

type user string

var (
	flagAdminUsers = flag.String("admins", "", "comma-separated list of admin usernames")
)

var isAdminMap map[user]bool

func initUsers() {
	isAdminMap = make(map[user]bool)
	for _, un := range strings.Split(*flagAdminUsers, ",") {
		isAdminMap[user(un)] = true
	}
}

func (u user) IsAdmin() bool {
	return isAdminMap[u]
}

func (u user) CanAccessJob(jobOwner user) bool {
	return u == jobOwner || u.IsAdmin()
}
