// Copyright (c) 2021 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux

package vms

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	expect "github.com/google/goexpect"
	"golang.org/x/crypto/ssh"
	"inet.af/netaddr"
)

const timeout = 15 * time.Second

func (h Harness) testPing(t *testing.T, ipAddr netaddr.IP, cli *ssh.Client) {
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("can't open ssh session: %v", err)
	}
	defer sess.Close()

	outp, err := sess.CombinedOutput(fmt.Sprintf("tailscale ping -c 1 %s", ipAddr))
	if err != nil {
		t.Fatalf("can't get ping output: %v", err)
	}
	t.Log(string(outp))

	if !bytes.Contains(outp, []byte("pong")) {
		t.Fatal("no pong")
	}

	runTestCommands(t, timeout, cli, []expect.Batcher{
		// NOTE(Xe): the ping command is inconsistent across distros. Joy.
		&expect.BSnd{S: fmt.Sprintf("ping -c 1 %[1]s || ping -6 -c 1 %[1]s || ping6 -c 1 %[1]s\n", ipAddr)},
		&expect.BExp{R: `bytes`},
	})
}

func (h Harness) testOutgoingTCP(t *testing.T, ipAddr netaddr.IP, cli *ssh.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Logf("http connection from %s", r.RemoteAddr)
			cancel()
			fmt.Fprintln(w, "connection established")
		}),
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("::", "0"))
	if err != nil {
		t.Fatalf("can't make HTTP server: %v", err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	go s.Serve(ln)

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("can't open ssh session: %v", err)
	}

	cmd := fmt.Sprintf("curl -s -f http://%s\n", net.JoinHostPort(ipAddr.String(), port))
	t.Logf("running: %s", cmd)
	outp, err := sess.CombinedOutput(cmd)
	if err != nil {
		t.Log(string(outp))
		t.Fatalf("can't connect to http server: %v", err)
	}

	if msg := string(bytes.TrimSpace(outp)); !strings.Contains(msg, "connection established") {
		t.Fatalf("wanted %q, got: %q", "connection established", msg)
	}
	<-ctx.Done()
}
