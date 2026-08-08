package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestApprovalBusLogInfoExcludesRedisCredentials(t *testing.T) {
	const dsn = "rediss://redis-user:super-secret@redis.internal:6380/7"
	opts, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("ParseURL() error = %v", err)
	}

	info := newApprovalBusLogInfo(opts)
	if info.Address != "redis.internal:6380" || info.DB != 7 || !info.TLS {
		t.Fatalf("newApprovalBusLogInfo() = %+v", info)
	}

	logged := fmt.Sprintf("%+v", info)
	for _, secret := range []string{"redis-user", "super-secret", dsn} {
		if strings.Contains(logged, secret) {
			t.Fatalf("safe Redis log metadata contains credential %q: %s", secret, logged)
		}
	}
}

func TestApprovalBusLogInfoHandlesNilOptions(t *testing.T) {
	if got := newApprovalBusLogInfo(nil); got != (approvalBusLogInfo{}) {
		t.Fatalf("newApprovalBusLogInfo(nil) = %+v", got)
	}
}
