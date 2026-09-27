package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseShare covers the command line form of --share.
func TestParseShare(t *testing.T) {
	cases := []struct {
		input    string
		path     string
		target   string
		readOnly bool
	}{
		{"/srv/data", "/srv/data", "", false},
		{"/srv/data:/data", "/srv/data", "/data", false},
		{"/srv/data:/data:ro", "/srv/data", "/data", true},
		{"/srv/data:/data:rw", "/srv/data", "/data", false},
	}
	for _, c := range cases {
		share, err := ParseShare(c.input)
		if err != nil {
			t.Fatalf("ParseShare(%q): %v", c.input, err)
		}
		if share.Path != c.path || share.Target != c.target || share.ReadOnly != c.readOnly {
			t.Errorf("ParseShare(%q) = %+v", c.input, share)
		}
	}
}

// TestParseShareDefaultTarget documents where a share appears by default.
func TestParseShareDefaultTarget(t *testing.T) {
	share, err := ParseShare("/srv/web-assets")
	if err != nil {
		t.Fatal(err)
	}
	if got := share.MountTarget(); got != "/shared/web-assets" {
		t.Errorf("MountTarget = %q, want /shared/web-assets", got)
	}
}

// TestParseShareRejectsInvalidInput makes a typo fail instead of mounting
// something unexpected into the container.
func TestParseShareRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{
		"",
		"relative/path",
		"/srv/data:relative",
		"/srv/data:/:ro",           // the container root
		"/srv/data:/proc",          // a system directory
		"/srv/data:/data:rwx",      // unknown option
		"/srv/data:/data:ro:extra", // too many fields
	} {
		if _, err := ParseShare(input); err == nil {
			t.Errorf("ParseShare(%q) should fail", input)
		}
	}
}

// TestShareJSONForms verifies that a share can be written either as a plain
// path or as an object, which keeps the common case short.
func TestShareJSONForms(t *testing.T) {
	var shares []Share
	if err := json.Unmarshal([]byte(`["/srv/a", {"path": "/srv/b", "target": "/data", "read_only": true}]`), &shares); err != nil {
		t.Fatal(err)
	}
	if len(shares) != 2 {
		t.Fatalf("parsed %d shares", len(shares))
	}
	if shares[0].Path != "/srv/a" || shares[0].MountTarget() != "/shared/a" {
		t.Errorf("short form = %+v", shares[0])
	}
	if shares[1].Path != "/srv/b" || shares[1].Target != "/data" || !shares[1].ReadOnly {
		t.Errorf("object form = %+v", shares[1])
	}

	// The short form is written back for a plain writable share.
	out, err := json.Marshal(Share{Path: "/srv/a"})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `"/srv/a"` {
		t.Errorf("Marshal = %s, want \"/srv/a\"", out)
	}
}

// TestDuplicateShareTargetsAreRejected protects a container from two mounts
// racing for the same destination.
func TestDuplicateShareTargetsAreRejected(t *testing.T) {
	c := Default()
	c.Shares = []Share{
		{Path: "/srv/one", Target: "/data"},
		{Path: "/srv/two", Target: "/data"},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("two shares at the same target must be rejected")
	}
	if !strings.Contains(err.Error(), "/data") {
		t.Errorf("the error should name the target: %v", err)
	}
}

// TestTmpSizeValidation covers the tmp_size values: an absolute size, a
// percentage, and the typos that must be rejected before a container fails to
// start.
func TestTmpSizeValidation(t *testing.T) {
	valid := []string{"", "8G", "512M", "1T", "50%", "100%", "1%", "disk", "DISK"}
	for _, value := range valid {
		c := Default()
		c.TmpSize = value
		if err := c.Validate(); err != nil {
			t.Errorf("TmpSize %q should be accepted: %v", value, err)
		}
	}
	invalid := []string{"0", "0%", "-1G", "101%", "abc", "8GB x"}
	for _, value := range invalid {
		c := Default()
		c.TmpSize = value
		if err := c.Validate(); err == nil {
			t.Errorf("TmpSize %q should be rejected", value)
		}
	}
}
