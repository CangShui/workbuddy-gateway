package main

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

func TestDisabledAutomaticPriceProbeStopsScheduler(t *testing.T) {
	oldDisabled, oldWriter := cfg.DisablePriceProbes, log.Writer()
	var audit bytes.Buffer
	cfg.DisablePriceProbes = true
	log.SetOutput(&audit)
	t.Cleanup(func() {
		cfg.DisablePriceProbes = oldDisabled
		log.SetOutput(oldWriter)
	})
	done := make(chan struct{})
	go func() {
		modelPriceProbeLoop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled price probe scheduler did not exit before starting any model requests")
	}
	for _, marker := range []string{"自动探测调度", "已关闭", "不会自动请求模型"} {
		if !strings.Contains(audit.String(), marker) {
			t.Errorf("missing probe shutdown audit marker %q", marker)
		}
	}
}
