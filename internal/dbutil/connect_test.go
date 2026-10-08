package dbutil

import (
	"strings"
	"testing"
)

func TestParsePoolConfigRefusesUnsafeConfigWithoutLeakingSecrets(t *testing.T) {
	for _, dsn := range []string{
		"postgres://localhost/observe?password=fixture-secret&port=invalid",
		"postgres://localhost/observe?%70assword=fixture-secret&sslmode=invalid",
		"password='fixture-secret\\",
		"postgres://localhost/observe?pool_health_check_period=0",
		"postgres://localhost/observe?pool_health_check_period=-1s",
		"postgres://localhost/observe?pool_max_conn_lifetime_jitter=-1s",
		"postgres://localhost/observe?pool_min_conns=-1",
		"postgres://localhost/observe?pool_min_conns=10&pool_max_conns=1",
		"postgres://localhost/observe?channel_binding=require",
	} {
		_, err := ParsePoolConfig(dsn)
		if err == nil {
			t.Errorf("invalid fixture config accepted")
		}
		if err != nil && strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("credential leaked")
		}
	}
	if _, err := ParsePoolConfig("postgres://nucleus@localhost:55447/observe?sslmode=disable"); err != nil {
		t.Fatal(err)
	}
}
