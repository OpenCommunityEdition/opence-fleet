package config

import (
	"flag"
	"testing"
)

func TestLoadServeJournal(t *testing.T) {
	load := func(value string) (*ServeConfig, error) {
		set := flag.NewFlagSet("serve", flag.ContinueOnError)
		values := Flags(set, Serve)
		if err := set.Parse([]string{"--db-journal", value}); err != nil {
			t.Fatal(err)
		}
		return LoadServe(values)
	}
	c, err := load("delete")
	if err != nil {
		t.Fatal(err)
	}
	if c.DBJournal != "delete" {
		t.Fatalf("journal = %q", c.DBJournal)
	}
	if _, err := load("not-a-journal"); err == nil {
		t.Fatal("invalid journal mode accepted")
	}
}
