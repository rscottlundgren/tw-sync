package main

import (
	"strings"
	"testing"
)

func TestSyncRCHasNoLiteralSecret(t *testing.T) {
	rc := syncRC("harvard", "bucket", "us-east-1")
	for _, want := range []string{
		"sync.aws.bucket=bucket",
		"sync.aws.default_credentials=true",
		"sync.encryption_secret=$TW_SYNC_SECRET",
		"recurrence=off",
	} {
		if !strings.Contains(rc, want) {
			t.Errorf("sync.rc is missing %q", want)
		}
	}
	for _, banned := range []string{"sync.aws.profile", "access_key"} {
		if strings.Contains(rc, banned) {
			t.Errorf("sync.rc must not set %q", banned)
		}
	}
}

func TestSyncRCRecurrenceOnPrimaryOnly(t *testing.T) {
	if !strings.Contains(syncRC("per", "b", "r"), "recurrence=on") {
		t.Error("per should generate recurring tasks")
	}
}

func TestCleanEnvDropsAWSSettings(t *testing.T) {
	got := cleanEnv([]string{"PATH=/bin", "AWS_PROFILE=x", "AWS_SESSION_TOKEN=y", "TW_SYNC_SECRET=z", "HOME=/h"})
	if len(got) != 2 || got[0] != "PATH=/bin" || got[1] != "HOME=/h" {
		t.Errorf("cleanEnv kept %v", got)
	}
}

func TestScrubRemovesSecrets(t *testing.T) {
	got := scrub("key AKIA123 failed", []string{"AKIA123", ""})
	if strings.Contains(got, "AKIA123") {
		t.Errorf("secret survived: %q", got)
	}
}

func TestKeychainSetRejectsQuoting(t *testing.T) {
	if err := keychainSet("x", `bad"value`); err == nil {
		t.Error("expected a rejection for a double quote")
	}
}
