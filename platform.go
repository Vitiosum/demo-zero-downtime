package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"runtime/debug"
)

// platform is what Clever Cloud injects into the environment of a running
// instance. Every field is empty outside the platform.
// Reference: https://www.clever.cloud/developers/doc/develop/common-configuration/environment-variables/reference/
type platform struct {
	Live           bool   // APP_ID is set: we run on Clever Cloud
	AppName        string // CC_APP_NAME
	AppID          string // APP_ID
	InstanceNumber string // INSTANCE_NUMBER: 0, 1… within the deployment
	InstanceName   string // CC_PRETTY_INSTANCE_NAME
	InstanceType   string // INSTANCE_TYPE: "production" (or "build")
	Commit         string // COMMIT_ID / CC_COMMIT_ID: the deployed commit, full length
	Deployment     string // CC_DEPLOYMENT_ID / DEPLOYMENT_ID
}

func loadPlatform(getenv func(string) string) platform {
	return platform{
		Live:           getenv("APP_ID") != "",
		AppName:        cut(getenv("CC_APP_NAME"), 40),
		AppID:          cut(getenv("APP_ID"), 48),
		InstanceNumber: cut(getenv("INSTANCE_NUMBER"), 4),
		InstanceName:   cut(getenv("CC_PRETTY_INSTANCE_NAME"), 40),
		InstanceType:   cut(getenv("INSTANCE_TYPE"), 20),
		Commit:         cut(firstEnv(getenv, "COMMIT_ID", "CC_COMMIT_ID"), 40),
		Deployment:     cut(firstEnv(getenv, "CC_DEPLOYMENT_ID", "DEPLOYMENT_ID"), 64),
	}
}

// ShortCommit is the 7-character form shown on the pages.
func (p platform) ShortCommit() string { return cut(p.Commit, 7) }

// Instance is "#0 · Pretty Name", or "" outside Clever Cloud.
func (p platform) Instance() string {
	if p.InstanceNumber == "" {
		return ""
	}
	if p.InstanceName == "" {
		return "#" + p.InstanceNumber
	}
	return "#" + p.InstanceNumber + " · " + p.InstanceName
}

// buildRevision is the git commit the Go toolchain stamped into the binary
// (vcs.revision), when the build ran inside a git checkout.
func buildRevision() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

// newBootID names this process: it changes at every start, so the tower can
// tell two processes apart even when they carry the same release.
func newBootID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return "000000"
	}
	return hex.EncodeToString(b)
}

func firstEnv(getenv func(string) string, keys ...string) string {
	for _, k := range keys {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// cut returns s truncated to n runes.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// listenPort returns the port injected by Clever Cloud, or 8080 locally.
func listenPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}
