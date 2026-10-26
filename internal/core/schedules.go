package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// ScheduleActions is what a schedule can do: the bulk actions, and an
// export of the database.
var ScheduleActions = []struct{ ID, Name string }{
	{ActionDrainRestart, "Restart, drained"}, {ActionRestart, "Restart"}, {ActionStart, "Start"}, {ActionStop, "Stop"},
	{ActionDrainStop, "Stop, drained"}, {ActionSay, "Say"}, {ActionSetRotation, "Set the rotation"},
	{ActionChangeProfile, "Change the profile"}, {ActionUpdate, "Update (rolling)"}, {ActionCommand, "Run a command"},
	{"export", "Export the database"},
}

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Targets is a schedule's servers: some, and groups (resolved at each run).
type Targets struct {
	Slugs  []string `json:"slugs,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// ScheduleInput is a schedule's form.
type ScheduleInput struct {
	Name    string
	Cron    string
	Action  string
	Targets Targets
	Params  JobParams
	Enabled bool
}

// NextRun is a cron expression's next time after now, in the fleet's zone.
func (f *Fleet) NextRun(expression string, after time.Time) (time.Time, error) {
	schedule, err := cronParser.Parse(strings.TrimSpace(expression))
	if err != nil {
		return time.Time{}, Refuse(http.StatusBadRequest, "schema", "%q is not a cron expression (minute hour day month weekday, or @daily): %v", expression, err)
	}
	return schedule.Next(after.In(f.Location())), nil
}

func (f *Fleet) checkSchedule(in *ScheduleInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		return Refuse(http.StatusBadRequest, "schema", "a schedule's name is 1 to 64 characters")
	}
	valid := false
	for _, a := range ScheduleActions {
		if a.ID == in.Action {
			valid = true
		}
	}
	if !valid {
		return Refuse(http.StatusBadRequest, "schema", "%q is not a schedule's action", in.Action)
	}
	if in.Action != "export" && len(in.Targets.Slugs) == 0 && len(in.Targets.Groups) == 0 {
		return Refuse(http.StatusBadRequest, "schema", "choose servers or groups")
	}
	_, err := f.NextRun(in.Cron, f.Now())
	return err
}

// CreateSchedule makes a schedule.
func (f *Fleet) CreateSchedule(ctx context.Context, actor string, in ScheduleInput) (int64, error) {
	if err := f.checkSchedule(&in); err != nil {
		return 0, err
	}
	next, _ := f.NextRun(in.Cron, f.Now())
	targets, _ := json.Marshal(in.Targets)
	params, _ := json.Marshal(in.Params)
	id, err := f.Store.CreateSchedule(ctx, db.CreateScheduleParams{Name: in.Name, Cron: in.Cron, Action: in.Action,
		Targets: string(targets), Params: string(params), Enabled: boolInt(in.Enabled), NextRun: ptrInt(next.Unix())})
	if err != nil {
		return 0, Refuse(http.StatusConflict, "taken", "a schedule is named %s already", in.Name)
	}
	f.Audit(ctx, actor, "schedule.create", in.Name, map[string]any{"cron": in.Cron, "action": in.Action, "targets": in.Targets})
	return id, nil
}

// UpdateSchedule changes a schedule.
func (f *Fleet) UpdateSchedule(ctx context.Context, actor string, id int64, in ScheduleInput) error {
	if err := f.checkSchedule(&in); err != nil {
		return err
	}
	next, _ := f.NextRun(in.Cron, f.Now())
	targets, _ := json.Marshal(in.Targets)
	params, _ := json.Marshal(in.Params)
	if err := f.Store.UpdateSchedule(ctx, db.UpdateScheduleParams{Name: in.Name, Cron: in.Cron, Action: in.Action,
		Targets: string(targets), Params: string(params), Enabled: boolInt(in.Enabled), NextRun: ptrInt(next.Unix()), ID: id}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "schedule.update", in.Name, map[string]any{"cron": in.Cron, "action": in.Action, "enabled": in.Enabled})
	return nil
}

// DeleteSchedule removes a schedule.
func (f *Fleet) DeleteSchedule(ctx context.Context, actor string, id int64) error {
	s, err := f.Store.GetSchedule(ctx, id)
	if err != nil {
		return NotFound("the schedule")
	}
	if err := f.Store.DeleteSchedule(ctx, id); err != nil {
		return err
	}
	f.Audit(ctx, actor, "schedule.delete", s.Name, nil)
	return nil
}

// ReadSchedule reads a schedule's targets and parameters.
func ReadSchedule(s db.Schedule) (Targets, JobParams) {
	var t Targets
	var p JobParams
	json.Unmarshal([]byte(s.Targets), &t)
	json.Unmarshal([]byte(s.Params), &p)
	return t, p
}

// RunSchedule runs a schedule now: a job (actor schedule:<name>), or an
// export.
func (f *Fleet) RunSchedule(ctx context.Context, id int64) (int64, error) {
	s, err := f.Store.GetSchedule(ctx, id)
	if err != nil {
		return 0, NotFound("the schedule")
	}
	actor := "schedule:" + s.Name
	next, _ := f.NextRun(s.Cron, f.Now())
	if s.Action == "export" {
		path, err := f.Export(ctx)
		f.Store.UpdateScheduleRun(ctx, db.UpdateScheduleRunParams{LastRun: ptrInt(f.Now().Unix()), NextRun: ptrInt(next.Unix()), ID: id})
		if err == nil {
			f.Audit(ctx, actor, "export", path, nil)
		}
		return 0, err
	}
	targets, params := ReadSchedule(s)
	slugs := map[string]bool{}
	for _, slug := range targets.Slugs {
		slugs[slug] = true
	}
	for _, g := range targets.Groups {
		members, _ := f.Store.ListGroupMembers(ctx, g)
		for _, m := range members {
			slugs[m] = true
		}
	}
	params.Action = s.Action
	params.Slugs = nil
	for slug := range slugs {
		if _, err := f.Store.GetInstance(ctx, slug); err == nil {
			params.Slugs = append(params.Slugs, slug)
		}
	}
	sort.Strings(params.Slugs)
	params.Role = "admin"
	job, err := f.StartJob(ctx, actor, params)
	f.Store.UpdateScheduleRun(ctx, db.UpdateScheduleRunParams{LastRun: ptrInt(f.Now().Unix()), NextRun: ptrInt(next.Unix()),
		LastJob: &job, ID: id})
	return job, err
}

// scheduleLoop runs the schedules whose time came, each minute.
func (f *Fleet) scheduleLoop(ctx context.Context) {
	for {
		now := f.Now()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(now.Truncate(time.Minute).Add(time.Minute + time.Second))):
		}
		schedules, err := f.Store.ListSchedules(ctx)
		if err != nil {
			continue
		}
		for _, s := range schedules {
			if s.Enabled == 0 || s.NextRun == nil || *s.NextRun > f.Now().Unix() {
				continue
			}
			if _, err := f.RunSchedule(ctx, s.ID); err != nil {
				f.Log.Warn("a schedule failed", "schedule", s.Name, "err", err)
			}
		}
	}
}

// Export writes a consistent copy of the database to FLEET_DATA/exports
// (the newest 7 kept): its path.
func (f *Fleet) Export(ctx context.Context) (string, error) {
	dir := filepath.Join(f.Config.Data, "exports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "fleet-"+f.Now().UTC().Format("20060102-150405")+".db")
	if err := f.Store.Snapshot(ctx, path); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "fleet-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > 7 {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return path, nil
}

// ScheduleSummary is a schedule's line on its page.
func ScheduleSummary(s db.Schedule) string {
	t, _ := ReadSchedule(s)
	var parts []string
	if len(t.Groups) > 0 {
		parts = append(parts, "groups "+strings.Join(t.Groups, ", "))
	}
	if len(t.Slugs) > 0 {
		parts = append(parts, fmt.Sprintf("%d servers", len(t.Slugs)))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " and ")
}

// itoa is strconv's.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }
