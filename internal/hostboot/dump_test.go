package hostboot

import (
	"os"
	"testing"
)

func TestRenderIgnitionForManualInspection(t *testing.T) {
	if os.Getenv("LUX_DUMP_IGNITION") == "" {
		t.Skip("set LUX_DUMP_IGNITION to dump a rendered config for manual validation")
	}
	b, err := Ignition(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_test", HostName: "runner-a", EC2IMDS: "http://169.254.169.254"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("LUX_DUMP_IGNITION"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRenderScriptForManualInspection(t *testing.T) {
	if os.Getenv("LUX_DUMP_SCRIPT") == "" {
		t.Skip("set LUX_DUMP_SCRIPT to dump a rendered script for manual validation")
	}
	got := Script(Env{URL: "http://10.0.1.10:7070", HostToken: "luxh_test", HostName: "runner-a", EC2IMDS: "http://169.254.169.254"})
	if err := os.WriteFile(os.Getenv("LUX_DUMP_SCRIPT"), []byte(got), 0o644); err != nil {
		t.Fatal(err)
	}
}
