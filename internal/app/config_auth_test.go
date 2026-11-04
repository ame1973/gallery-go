package app

import (
	"testing"
	"time"
)

func TestSessionPolicyConfiguration(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "auth:\n  session_idle_timeout: 2h\n  session_absolute_timeout: 48h\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Idle != 2*time.Hour || c.Auth.Absolute != 48*time.Hour {
		t.Fatalf("policy %+v", c.Auth)
	}
	for _, text := range []string{"auth: {session_idle_timeout: 0s}", "auth: {session_idle_timeout: 25h}", "auth: {session_absolute_timeout: 721h}", "auth: {session_absolute_timeout: 1m}"} {
		if _, err := LoadConfig(writeConfig(t, text)); err == nil {
			t.Fatalf("invalid policy accepted: %s", text)
		}
	}
}
