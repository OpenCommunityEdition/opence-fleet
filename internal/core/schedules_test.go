package core

import (
	"context"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
)

func TestNextRunInTheFleetsZone(t *testing.T) {
	auckland, err := time.LoadLocation("Pacific/Auckland")
	if err != nil {
		t.Fatal(err)
	}
	kolkata := time.FixedZone("IST", 5*3600+1800)
	utc := func(s string) time.Time {
		at, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	cases := []struct {
		name       string
		zone       *time.Location
		expression string
		after      string
		want       string
	}{
		// 04:00 in Auckland (NZDT, +13) is 15:00 UTC the day before
		{"daily, summer time", auckland, "0 4 * * *", "2026-10-26 12:00", "2026-10-26 15:00"},
		// (the clocks went back on 5 April: +12 from then)
		{"daily, before the change", auckland, "0 4 * * *", "2026-04-03 12:00", "2026-04-03 15:00"},
		{"daily, after the change", auckland, "0 4 * * *", "2026-04-03 15:00", "2026-04-04 16:00"},
		{"a weekday", auckland, "30 3 * * 1", "2026-10-26 12:00", "2026-11-01 14:30"},
		{"@daily is local midnight", auckland, "@daily", "2026-10-26 12:00", "2026-10-27 11:00"},
		{"a zone of half hours", kolkata, "0 6 * * *", "2026-10-26 00:00", "2026-10-26 00:30"},
		{"every 15 minutes", kolkata, "*/15 * * * *", "2026-10-26 00:01", "2026-10-26 00:15"},
		{"UTC", time.UTC, "0 4 * * *", "2026-10-26 12:00", "2026-10-27 04:00"},
		// (no zone: UTC)
		{"no zone", nil, "0 4 * * *", "2026-10-26 12:00", "2026-10-27 04:00"},
	}
	f, _ := fleet(t)
	for _, c := range cases {
		f.Config.Timezone = c.zone
		next, err := f.NextRun(c.expression, utc(c.after))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if want := utc(c.want); !next.Equal(want) {
			t.Errorf("%s: %s, not %s", c.name, next.UTC().Format(time.DateTime), want.Format(time.DateTime))
		}
		if c.zone != nil && next.Location() != c.zone {
			t.Errorf("%s: in %s", c.name, next.Location())
		}
	}
}

func TestBadCron(t *testing.T) {
	f, _ := fleet(t)
	for _, expression := range []string{"", "61 * * * *", "* 25 * * *", "* * *", "0 4 * * * *", "every day", "@fortnightly"} {
		_, err := f.NextRun(expression, time.Now())
		if e, ok := AsError(err); !ok || e.Code != "schema" || e.Status != 400 {
			t.Errorf("%q: %v", expression, err)
		}
	}
}

func TestSchedules(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	auckland, _ := time.LoadLocation("Pacific/Auckland")
	f.Config.Timezone = auckland
	stop(f, time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC))
	node(t, f, "a", spec.DefaultNode())
	slugs := servers(t, f, profile(t, f, "Ranked"), 3, false, "a")
	if err := f.SetGroups(ctx, "user:paul", slugs[:2], "eu", true); err != nil {
		t.Fatal(err)
	}
	good := ScheduleInput{Name: "Nightly restart", Cron: "0 4 * * *", Action: ActionDrainRestart,
		Targets: Targets{Groups: []string{"eu"}}, Enabled: true}
	for _, c := range []struct {
		name string
		edit func(in *ScheduleInput)
	}{
		{"no name", func(in *ScheduleInput) { in.Name = " " }},
		{"a bad cron", func(in *ScheduleInput) { in.Cron = "0 4 * *" }},
		{"an unknown action", func(in *ScheduleInput) { in.Action = "reboot" }},
		{"no servers", func(in *ScheduleInput) { in.Targets = Targets{} }},
	} {
		in := good
		c.edit(&in)
		if _, err := f.CreateSchedule(ctx, "user:paul", in); err == nil {
			t.Errorf("%s: made", c.name)
		} else if e, ok := AsError(err); !ok || e.Code != "schema" {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	id, err := f.CreateSchedule(ctx, "user:paul", good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CreateSchedule(ctx, "user:paul", good); err == nil {
		t.Fatal("two schedules of a name")
	}
	s, _ := f.Store.GetSchedule(ctx, id)
	// 04:00 in Auckland, 15:00 UTC
	if s.NextRun == nil || *s.NextRun != time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC).Unix() {
		t.Fatalf("next run %v", s.NextRun)
	}
	if a, ok := audited(t, f, "schedule.create"); !ok || a.Actor != "user:paul" || a.Target != "Nightly restart" {
		t.Fatalf("%+v", a)
	}
	// a run is a job of the group's servers now, by the schedule
	job, err := f.RunSchedule(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, items, err := f.Job(ctx, job)
	if err != nil || len(items) != 2 || items[0].Slug != slugs[0] || items[1].Slug != slugs[1] {
		t.Fatalf("%+v %v", items, err)
	}
	if a, ok := audited(t, f, "job.start"); !ok || a.Actor != "schedule:Nightly restart" {
		t.Fatalf("%+v", a)
	}
	s, _ = f.Store.GetSchedule(ctx, id)
	if s.LastJob == nil || *s.LastJob != job || s.LastRun == nil {
		t.Fatalf("%+v", s)
	}
	if err := f.DeleteSchedule(ctx, "user:paul", id); err != nil {
		t.Fatal(err)
	}
	if e, ok := AsError(f.DeleteSchedule(ctx, "user:paul", id)); !ok || e.Code != "not_found" {
		t.Fatalf("%v", e)
	}
}
