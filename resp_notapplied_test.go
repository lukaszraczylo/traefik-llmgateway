package traefikllmgateway

import (
	"bufio"
	"fmt"
	"testing"
	"time"
)

func TestRESPPipelineNotApplied(t *testing.T) {
	incr := [][]string{{"INCRBY", "k", "1"}, {"EXPIRE", "k", "60"}}

	t.Run("dial failure was never written", func(t *testing.T) {
		ln := newFakeListener(t)
		addr := ln.Addr().String()
		_ = ln.Close()
		c := newRESPClient(addr, "", 0)
		_, err := c.pipeline(incr)
		if err == nil {
			t.Fatal("want an error dialing a dead address")
		}
		if !respNotApplied(err) {
			t.Errorf("respNotApplied(%v) = false, want true", err)
		}
	})

	t.Run("reply lost after bytes arrived is ambiguous", func(t *testing.T) {
		ln := newFakeListener(t)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = ln.Close()
			defer func() { _ = conn.Close() }()
			r := bufio.NewReader(conn)
			_, _ = readRESPCommand(r) // SELECT
			_, _ = conn.Write([]byte("+OK\r\n"))
			_, _ = readRESPCommand(r)
			_, _ = readRESPCommand(r)
			_, _ = conn.Write([]byte(":1\r\n"))
		}()
		c := newRESPClient(ln.Addr().String(), "", 0)
		_, err := c.pipeline(incr)
		if err == nil {
			t.Fatal("want an error")
		}
		if respNotApplied(err) {
			t.Errorf("respNotApplied(%v) = true, want false: the INCRBY may have landed", err)
		}
	})

	t.Run("timeout after the write is ambiguous", func(t *testing.T) {
		ln := newFakeListener(t)
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop) })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			r := bufio.NewReader(conn)
			_, _ = readRESPCommand(r) // SELECT
			_, _ = conn.Write([]byte("+OK\r\n"))
			_, _ = readRESPCommand(r)
			<-stop
		}()
		c := newRESPClient(ln.Addr().String(), "", 0)
		_, err := c.pipeline(incr)
		if err == nil {
			t.Fatal("want a timeout")
		}
		if respNotApplied(err) {
			t.Errorf("respNotApplied(%v) = true, want false", err)
		}
	})

	t.Run("nil and foreign errors are not marked", func(t *testing.T) {
		if respNotApplied(nil) || respNotApplied(fmt.Errorf("boom")) {
			t.Error("unmarked error reported as not applied")
		}
	})
}

func TestRedisStoreKeepsNotAppliedMarker(t *testing.T) {
	ln := newFakeListener(t)
	addr := ln.Addr().String()
	_ = ln.Close()
	s := newRedisStore(newRESPClient(addr, "", 0))
	entries := []counterIncr{{key: "k", delta: 1, ttl: time.Minute, enforceTTL: time.Minute}}
	if _, err := s.incrMulti(entries); err == nil || !respNotApplied(err) {
		t.Errorf("incrMulti error %v: respNotApplied = false, want true for a dial failure", err)
	}
	if _, _, err := s.incrAndGetMulti(entries, nil); err == nil || !respNotApplied(err) {
		t.Errorf("incrAndGetMulti error %v: respNotApplied = false, want true for a dial failure", err)
	}
}
