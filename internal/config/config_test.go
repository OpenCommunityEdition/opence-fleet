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
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	unset, err := LoadServe(Flags(set, Serve))
	if err != nil {
		t.Fatal(err)
	}
	if unset.DBJournal != "" {
		t.Fatalf("unset journal = %q, want empty (keep the database's)", unset.DBJournal)
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
