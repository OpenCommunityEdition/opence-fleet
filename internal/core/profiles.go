package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/OpenCommunityEdition/opence-fleet/internal/spec"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store"
	"github.com/OpenCommunityEdition/opence-fleet/internal/store/db"
)

// Profile is a profile with its settings and gametype files read.
type Profile struct {
	db.Profile
	Settings  map[string]any
	Gametypes map[string]string
}

// The password policies.
const (
	PasswordNone        = "none"
	PasswordFixed       = "fixed"
	PasswordPerInstance = "per_instance"
)

// Profile finds a profile.
func (f *Fleet) Profile(ctx context.Context, id int64) (Profile, error) {
	row, err := f.Store.GetProfile(ctx, id)
	if err != nil {
		return Profile{}, NotFound("the profile")
	}
	return f.readProfile(ctx, row)
}

func (f *Fleet) readProfile(ctx context.Context, row db.Profile) (Profile, error) {
	p := Profile{Profile: row, Settings: map[string]any{}, Gametypes: map[string]string{}}
	json.Unmarshal([]byte(row.Settings), &p.Settings)
	gametypes, err := f.Store.ListProfileGametypes(ctx, row.ID)
	if err != nil {
		return p, err
	}
	for _, g := range gametypes {
		p.Gametypes[g.Name] = g.Toml
	}
	return p, nil
}

// Profiles lists the profiles.
func (f *Fleet) Profiles(ctx context.Context) ([]Profile, error) {
	rows, err := f.Store.ListProfiles(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(rows))
	for _, r := range rows {
		p, err := f.readProfile(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ProfileInput is a profile's form.
type ProfileInput struct {
	Name, Description string
	Settings          map[string]any
	Rotation          string
	Region            string
	InitTxt           string
	Channel           string
	PasswordPolicy    string
	// the fixed password ("" keeps the one there)
	Password  string
	KeyID     string
	ApplyMode string
}

func (f *Fleet) checkProfile(ctx context.Context, in *ProfileInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 64 {
		return Refuse(http.StatusBadRequest, "schema", "a profile's name is 1 to 64 characters")
	}
	settings := map[string]any{}
	for name, value := range in.Settings {
		v, err := spec.Normalize(name, value)
		if err != nil {
			return Refuse(http.StatusBadRequest, "schema", "%v", err)
		}
		settings[name] = v
	}
	in.Settings = settings
	if len(in.Rotation) > 4096 {
		return Refuse(http.StatusBadRequest, "schema", "a rotation is 4096 characters at most")
	}
	if n := len(spec.RotationEntries(in.Rotation)); n > 64 {
		return Refuse(http.StatusBadRequest, "schema", "a rotation is 64 entries at most (%d)", n)
	}
	in.Region = strings.ToLower(strings.TrimSpace(in.Region))
	if in.Region != "" && !spec.KnownRegion(in.Region) {
		var known []string
		for _, r := range spec.Regions {
			known = append(known, r.Value)
		}
		return Refuse(http.StatusBadRequest, "schema", "%q is not a region: one of %s, or none", in.Region,
			strings.Join(known, ", "))
	}
	switch in.PasswordPolicy {
	case PasswordNone, PasswordFixed, PasswordPerInstance:
	case "":
		in.PasswordPolicy = PasswordNone
	default:
		return Refuse(http.StatusBadRequest, "schema", "the password policy is none, fixed or per_instance")
	}
	switch in.ApplyMode {
	case "live", "lobby":
	case "":
		in.ApplyMode = "live"
	default:
		return Refuse(http.StatusBadRequest, "schema", "the apply mode is live or lobby")
	}
	if in.Channel == "" {
		in.Channel = "latest"
	}
	if in.Channel != "latest" {
		if _, err := f.Store.GetRelease(ctx, in.Channel); err != nil {
			return Refuse(http.StatusBadRequest, "schema", "release %s is not known (fetch it on the Releases page)", in.Channel)
		}
	}
	if in.KeyID != "" {
		if _, err := f.Store.GetKey(ctx, in.KeyID); err != nil {
			return Refuse(http.StatusBadRequest, "schema", "no publisher key %s", in.KeyID)
		}
	}
	return nil
}

// CreateProfile makes a profile.
func (f *Fleet) CreateProfile(ctx context.Context, actor string, in ProfileInput) (int64, error) {
	if err := f.checkProfile(ctx, &in); err != nil {
		return 0, err
	}
	if _, err := f.Store.GetProfileByName(ctx, in.Name); err == nil {
		return 0, Refuse(http.StatusConflict, "taken", "a profile is named %s already", in.Name)
	}
	data, _ := json.Marshal(in.Settings)
	id, err := f.Store.CreateProfile(ctx, db.CreateProfileParams{Name: in.Name, Description: in.Description, Settings: string(data),
		Rotation: in.Rotation, Region: in.Region, InitTxt: in.InitTxt, Channel: in.Channel, PasswordPolicy: in.PasswordPolicy,
		KeyID: ptrString(in.KeyID), ApplyMode: in.ApplyMode, UpdatedAt: f.Now().Unix()})
	if err != nil {
		return 0, err
	}
	if in.PasswordPolicy == PasswordFixed && in.Password != "" {
		f.Store.DB.ExecContext(ctx, `UPDATE profiles SET password_enc = ? WHERE id = ?`,
			f.Key.SealString(in.Password, fmt.Sprintf("profile:%d:password", id)), id)
	}
	f.Audit(ctx, actor, "profile.create", in.Name, map[string]any{"settings": redactSettings(in.Settings), "rotation": in.Rotation})
	return id, nil
}

// UpdateProfile changes a profile, and every instance that follows it
// (each gets a new generation: its agent applies it live where it can).
func (f *Fleet) UpdateProfile(ctx context.Context, actor string, id int64, in ProfileInput) error {
	old, err := f.Profile(ctx, id)
	if err != nil {
		return err
	}
	if err := f.checkProfile(ctx, &in); err != nil {
		return err
	}
	if other, err := f.Store.GetProfileByName(ctx, in.Name); err == nil && other.ID != id {
		return Refuse(http.StatusConflict, "taken", "a profile is named %s already", in.Name)
	}
	password := old.PasswordEnc
	if in.PasswordPolicy == PasswordFixed && in.Password != "" {
		password = f.Key.SealString(in.Password, fmt.Sprintf("profile:%d:password", id))
	}
	if in.PasswordPolicy != PasswordFixed {
		password = nil
	}
	data, _ := json.Marshal(in.Settings)
	if err := f.Store.UpdateProfile(ctx, db.UpdateProfileParams{Name: in.Name, Description: in.Description, Settings: string(data),
		Rotation: in.Rotation, Region: in.Region, InitTxt: in.InitTxt, Channel: in.Channel, PasswordPolicy: in.PasswordPolicy,
		PasswordEnc: password, KeyID: ptrString(in.KeyID), ApplyMode: in.ApplyMode, UpdatedAt: f.Now().Unix(), ID: id}); err != nil {
		return err
	}
	if in.PasswordPolicy == PasswordPerInstance && old.PasswordPolicy != PasswordPerInstance {
		followers, _ := f.Store.ListInstancesByProfile(ctx, &id)
		for _, i := range followers {
			if len(i.PasswordEnc) == 0 {
				f.Store.SetInstancePassword(ctx, db.SetInstancePasswordParams{
					PasswordEnc: f.Key.SealString(Password(10), "instance:"+i.Slug+":password"), Slug: i.Slug})
			}
		}
	}
	f.Audit(ctx, actor, "profile.update", in.Name, map[string]any{"settings": redactSettings(in.Settings), "rotation": in.Rotation,
		"channel": in.Channel})
	return f.applyProfile(ctx, id)
}

// applyProfile gives the profile's followers a new generation and pushes
// their nodes' specs.
func (f *Fleet) applyProfile(ctx context.Context, id int64) error {
	if err := f.Store.BumpProfileGenerations(ctx, &id); err != nil {
		return err
	}
	followers, err := f.Store.ListInstancesByProfile(ctx, &id)
	if err != nil {
		return err
	}
	nodes := map[int64]bool{}
	for _, i := range followers {
		nodes[i.NodeID] = true
		f.Live.Notify(liveChange("instance", i.Slug))
	}
	for n := range nodes {
		f.Push(n)
	}
	return nil
}

// DeleteProfile removes a profile nobody follows.
func (f *Fleet) DeleteProfile(ctx context.Context, actor string, id int64) error {
	p, err := f.Profile(ctx, id)
	if err != nil {
		return err
	}
	n, err := f.Store.CountProfileFollowers(ctx, &id)
	if err != nil {
		return err
	}
	if n > 0 {
		return Refuse(http.StatusConflict, "in_use", "%d servers follow %s: give them another profile first", n, p.Name)
	}
	if err := f.Store.DeleteProfile(ctx, id); err != nil {
		return err
	}
	f.Audit(ctx, actor, "profile.delete", p.Name, nil)
	return nil
}

// DuplicateProfile copies a profile under a new name.
func (f *Fleet) DuplicateProfile(ctx context.Context, actor string, id int64) (int64, error) {
	p, err := f.Profile(ctx, id)
	if err != nil {
		return 0, err
	}
	name := p.Name + " (copy)"
	for n := 2; ; n++ {
		if _, err := f.Store.GetProfileByName(ctx, name); store.NotFound(err) {
			break
		}
		name = fmt.Sprintf("%s (copy %d)", p.Name, n)
	}
	password, _ := f.Key.OpenString(p.PasswordEnc, fmt.Sprintf("profile:%d:password", p.ID))
	copyID, err := f.CreateProfile(ctx, actor, ProfileInput{Name: name, Description: p.Description, Settings: p.Settings,
		Rotation: p.Rotation, Region: p.Region, InitTxt: p.InitTxt, Channel: p.Channel, PasswordPolicy: p.PasswordPolicy,
		Password: password, KeyID: deref(p.KeyID), ApplyMode: p.ApplyMode})
	if err != nil {
		return 0, err
	}
	for gname, text := range p.Gametypes {
		f.Store.UpsertProfileGametype(ctx, db.UpsertProfileGametypeParams{ProfileID: copyID, Name: gname, Toml: text})
	}
	return copyID, nil
}

var gametypeName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// gametypeTables are a gametype file's tables (docs/dedicated_server.md).
var gametypeTables = map[string]bool{"universal": true, "options": true, "ctf": true, "slayer": true, "oddball": true,
	"king": true, "race": true}

// CheckGametype checks a gametype file as far as the fleet can without a
// server: its name, TOML that parses, its base, its tables. (The server's
// PUT /api/gametypes checks every rule against its schema.)
func CheckGametype(name, text string) error {
	if !gametypeName.MatchString(name) {
		return Refuse(http.StatusBadRequest, "schema", "a gametype's name is a-z, 0-9, '_' and '-', 32 at most")
	}
	if len(text) > 64<<10 {
		return Refuse(http.StatusBadRequest, "schema", "a gametype file is 64 KB at most")
	}
	var parsed map[string]any
	meta, err := toml.Decode(text, &parsed)
	if err != nil {
		return Refuse(http.StatusBadRequest, "invalid", "the TOML does not parse: %v", err)
	}
	_ = meta
	base, ok := parsed["base"].(string)
	if !ok || base == "" {
		return Refuse(http.StatusBadRequest, "invalid", "a gametype file names its base (base = \"team_slayer\")")
	}
	for key, value := range parsed {
		switch key {
		case "base", "name", "description":
			continue
		}
		if _, isTable := value.(map[string]any); !isTable || !gametypeTables[key] {
			return Refuse(http.StatusBadRequest, "invalid", "%s is not a gametype file's table (universal, options, ctf, slayer, oddball, king, race)", key)
		}
	}
	if n, ok := parsed["name"].(string); ok && len(n) > 11 {
		return Refuse(http.StatusBadRequest, "invalid", "the name the game shows is 11 characters at most")
	}
	return nil
}

// SetProfileGametype adds or replaces one of a profile's gametype files.
func (f *Fleet) SetProfileGametype(ctx context.Context, actor string, id int64, name, text string) error {
	p, err := f.Profile(ctx, id)
	if err != nil {
		return err
	}
	if err := CheckGametype(name, text); err != nil {
		return err
	}
	if err := f.Store.UpsertProfileGametype(ctx, db.UpsertProfileGametypeParams{ProfileID: id, Name: name, Toml: text}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "profile.gametype", p.Name+"/"+name, nil)
	return f.applyProfile(ctx, id)
}

// DeleteProfileGametype removes one of a profile's gametype files.
func (f *Fleet) DeleteProfileGametype(ctx context.Context, actor string, id int64, name string) error {
	p, err := f.Profile(ctx, id)
	if err != nil {
		return err
	}
	if err := f.Store.DeleteProfileGametype(ctx, db.DeleteProfileGametypeParams{ProfileID: id, Name: name}); err != nil {
		return err
	}
	f.Audit(ctx, actor, "profile.gametype_delete", p.Name+"/"+name, nil)
	return f.applyProfile(ctx, id)
}

// Followers lists the instances that follow a profile.
func (f *Fleet) Followers(ctx context.Context, id int64) ([]db.Instance, error) {
	return f.Store.ListInstancesByProfile(ctx, &id)
}

// Groups lists the groups and their members.
func (f *Fleet) Groups(ctx context.Context) ([]db.Group, map[string][]string, error) {
	groups, err := f.Store.ListGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	pairs, err := f.Store.ListInstanceGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	members := map[string][]string{}
	for _, p := range pairs {
		members[p.GroupName] = append(members[p.GroupName], p.Slug)
	}
	return groups, members, nil
}

// SetGroups gives instances a group (add) or takes it off them.
func (f *Fleet) SetGroups(ctx context.Context, actor string, slugs []string, group string, add bool) error {
	group = groupName(group)
	if group == "" {
		return Refuse(http.StatusBadRequest, "schema", "a group's name is letters, digits and '-'")
	}
	if add {
		if err := f.Store.UpsertGroup(ctx, db.UpsertGroupParams{Name: group}); err != nil {
			return err
		}
	}
	for _, slug := range slugs {
		if add {
			f.Store.AddInstanceGroup(ctx, db.AddInstanceGroupParams{Slug: slug, GroupName: group})
		} else {
			f.Store.RemoveInstanceGroup(ctx, db.RemoveInstanceGroupParams{Slug: slug, GroupName: group})
		}
		f.Live.Notify(liveChange("instance", slug))
	}
	action := "group.add"
	if !add {
		action = "group.remove"
	}
	f.Audit(ctx, actor, action, group, map[string]any{"servers": slugs})
	return nil
}

// GroupMembers is a group's instances.
func (f *Fleet) GroupMembers(ctx context.Context, group string) ([]string, error) {
	return f.Store.ListGroupMembers(ctx, group)
}

// DeleteGroup removes a group (its servers stay).
func (f *Fleet) DeleteGroup(ctx context.Context, actor, group string) error {
	if err := f.Store.DeleteGroup(ctx, group); err != nil {
		return err
	}
	f.Audit(ctx, actor, "group.delete", group, nil)
	return nil
}
