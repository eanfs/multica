package model

import (
	"errors"
	"strings"
	"testing"
)

const validLayout = `{"version":1,"namespace":"local","fleet_id":"fleet-1","node_id":"node-1","daemon_id":"daemon-1","data_mount":"/data","node_home":"/data/home","workspaces_root":"/data/workspaces"}`

func TestValidateLayoutManifest(t *testing.T) {
	want := LayoutIdentity{Namespace: "local", FleetID: "fleet-1", NodeID: "node-1", DaemonID: "daemon-1"}
	if err := ValidateLayoutManifest([]byte(validLayout), want); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"case variant version": strings.Replace(validLayout, `"version"`, `"Version"`, 1),
		"duplicate identity":   strings.Replace(validLayout, `"node_id":"node-1"`, `"node_id":"other","node_id":"node-1"`, 1),
		"malformed":            "{",
		"null":                 "null",
		"array":                "[]",
		"trailing":             validLayout + "{}",
		"unsupported version":  strings.Replace(validLayout, `"version":1`, `"version":2`, 1),
		"version type":         strings.Replace(validLayout, `"version":1`, `"version":"1"`, 1),
		"unknown":              strings.Replace(validLayout, `"version":1`, `"version":1,"extra":true`, 1),
	}
	fields := map[string]string{"namespace": "local", "fleet_id": "fleet-1", "node_id": "node-1", "daemon_id": "daemon-1", "data_mount": "/data", "node_home": "/data/home", "workspaces_root": "/data/workspaces"}
	for field, value := range fields {
		pair := `"` + field + `":"` + value + `"`
		cases[field+" mismatch"] = strings.Replace(validLayout, pair, `"`+field+`":"other"`, 1)
		cases[field+" empty"] = strings.Replace(validLayout, pair, `"`+field+`":""`, 1)
		cases[field+" null"] = strings.Replace(validLayout, pair, `"`+field+`":null`, 1)
		cases[field+" missing"] = strings.Replace(strings.Replace(validLayout, pair+",", "", 1), ","+pair, "", 1)
	}
	cases["version missing"] = strings.Replace(validLayout, `"version":1,`, "", 1)
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateLayoutManifest([]byte(raw), want); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestValidateLayoutManifestRejectsIncompleteExpectedIdentity(t *testing.T) {
	for _, field := range []string{"namespace", "fleet", "node", "daemon"} {
		t.Run(field, func(t *testing.T) {
			want := LayoutIdentity{Namespace: "local", FleetID: "fleet-1", NodeID: "node-1", DaemonID: "daemon-1"}
			switch field {
			case "namespace":
				want.Namespace = ""
			case "fleet":
				want.FleetID = ""
			case "node":
				want.NodeID = ""
			case "daemon":
				want.DaemonID = ""
			}
			if err := ValidateLayoutManifest([]byte(validLayout), want); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
