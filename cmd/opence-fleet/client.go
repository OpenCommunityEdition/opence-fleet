package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/config"
)

// api is the command line's client of /api/v1.
type api struct {
	base, token string
	http        *http.Client
}

func newAPI(set *flag.FlagSet, args []string) (*api, error) {
	values := config.Flags(set, config.Client)
	if err := set.Parse(args); err != nil {
		return nil, err
	}
	a := &api{base: strings.TrimRight(values.Get("FLEET_URL"), "/"), token: values.Get("FLEET_TOKEN"),
		http: &http.Client{Timeout: 2 * time.Minute}}
	if a.token == "" {
		return nil, errors.New("FLEET_TOKEN is needed: an API token from the Team page")
	}
	return a, nil
}

func (a *api) call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, _ := json.Marshal(in)
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, a.base+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+a.token)
	request.Header.Set("Accept", "application/json")
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := a.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("%s (%s)", e.Error.Message, e.Error.Code)
		}
		return fmt.Errorf("the fleet answered %s", response.Status)
	}
	if out != nil {
		if raw, ok := out.(*json.RawMessage); ok {
			*raw = data
			return nil
		}
		return json.Unmarshal(data, out)
	}
	return nil
}

func printJSON(raw json.RawMessage) {
	var buffer bytes.Buffer
	json.Indent(&buffer, raw, "", "  ")
	fmt.Println(buffer.String())
}

// client runs a command of the command line.
// clientCommands are the command line's commands of the API.
var clientCommands = map[string]bool{"servers": true, "nodes": true, "jobs": true, "start": true, "stop": true,
	"restart": true, "say": true, "command": true, "update": true, "library": true, "export": true, "import": true}

func client(command string, args []string) error {
	if !clientCommands[command] {
		return usageError(fmt.Sprintf("unknown command %q (opence-fleet help)", command))
	}
	set := flag.NewFlagSet(command, flag.ExitOnError)
	asJSON := set.Bool("json", false, "print the JSON answer")
	group := set.String("group", "", "the servers of a group")
	drain := set.Bool("drain", false, "restart or stop once the game ends (rolling)")
	channel := set.String("channel", "latest", "the release to update to")
	after := set.String("after", "game", "a drain's moment: game, empty or now")
	keys := set.Bool("keys", false, "export: with the publisher keys' seeds")
	a, err := newAPI(set, args)
	if err != nil {
		return err
	}
	rest := set.Args()
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	switch command {
	case "servers":
		var raw json.RawMessage
		path := "/api/v1/instances"
		if *group != "" {
			path += "?group=" + url.QueryEscape(*group)
		}
		if err := a.call(http.MethodGet, path, nil, &raw); err != nil {
			return err
		}
		if *asJSON {
			printJSON(raw)
			return nil
		}
		var answer struct {
			Instances []struct {
				Slug, Name, Node, State, Game, Build string
				Players                              int `json:"players"`
				Max                                  int `json:"max_players"`
			} `json:"instances"`
		}
		json.Unmarshal(raw, &answer)
		fmt.Fprintln(w, "SERVER\tNODE\tSTATE\tPLAYERS\tGAME\tRELEASE")
		for _, i := range answer.Instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d/%d\t%s\t%s\n", i.Slug, i.Node, i.State, i.Players, i.Max, i.Game, i.Build)
		}
	case "nodes":
		var raw json.RawMessage
		if err := a.call(http.MethodGet, "/api/v1/nodes", nil, &raw); err != nil {
			return err
		}
		if *asJSON {
			printJSON(raw)
			return nil
		}
		var answer struct {
			Nodes []struct {
				Name    string `json:"name"`
				Online  bool   `json:"online"`
				Servers int    `json:"servers"`
				Load    string `json:"load"`
				Memory  string `json:"memory"`
				Version string `json:"agent_version"`
			} `json:"nodes"`
		}
		json.Unmarshal(raw, &answer)
		fmt.Fprintln(w, "NODE\tONLINE\tSERVERS\tLOAD\tMEMORY\tAGENT")
		for _, n := range answer.Nodes {
			fmt.Fprintf(w, "%s\t%v\t%d\t%s\t%s\t%s\n", n.Name, n.Online, n.Servers, n.Load, n.Memory, n.Version)
		}
	case "jobs":
		var raw json.RawMessage
		if err := a.call(http.MethodGet, "/api/v1/jobs", nil, &raw); err != nil {
			return err
		}
		printJSON(raw)
	case "start", "stop", "restart":
		if len(rest) == 0 && *group == "" {
			return fmt.Errorf("usage: opence-fleet %s <server>... | --group <g>", command)
		}
		action := command
		if *drain && command == "restart" {
			action = "drain_restart"
		} else if *drain && command == "stop" {
			action = "drain_stop"
		}
		return a.job(action, rest, *group, map[string]any{"after": *after}, w)
	case "say":
		text := strings.Join(rest, " ")
		if *group == "" {
			var answer struct {
				Instances []struct{ Slug string } `json:"instances"`
			}
			if err := a.call(http.MethodGet, "/api/v1/instances", nil, &answer); err != nil {
				return err
			}
			var slugs []string
			for _, i := range answer.Instances {
				slugs = append(slugs, i.Slug)
			}
			return a.job("say", slugs, "", map[string]any{"text": text}, w)
		}
		return a.job("say", nil, *group, map[string]any{"text": text}, w)
	case "command":
		if len(rest) < 2 {
			return errors.New("usage: opence-fleet command <server> <line>")
		}
		var answer struct {
			Lines []string `json:"lines"`
		}
		if err := a.call(http.MethodPost, "/api/v1/instances/"+url.PathEscape(rest[0])+"/command",
			map[string]string{"line": strings.Join(rest[1:], " ")}, &answer); err != nil {
			return err
		}
		fmt.Println(strings.Join(answer.Lines, "\n"))
	case "update":
		var slugs []string
		if *group == "" && len(rest) == 0 {
			var answer struct {
				Instances []struct{ Slug string } `json:"instances"`
			}
			if err := a.call(http.MethodGet, "/api/v1/instances", nil, &answer); err != nil {
				return err
			}
			for _, i := range answer.Instances {
				slugs = append(slugs, i.Slug)
			}
		}
		return a.job("update", append(slugs, rest...), *group, map[string]any{"channel": *channel, "after": *after}, w)
	case "library":
		if len(rest) != 2 || rest[0] != "add" {
			return errors.New("usage: opence-fleet library add <folder> (a folder on the control plane's machine)")
		}
		folder, _ := filepath.Abs(rest[1])
		var raw json.RawMessage
		if err := a.call(http.MethodPost, "/api/v1/library/folder", map[string]string{"path": folder}, &raw); err != nil {
			return err
		}
		printJSON(raw)
	case "export":
		var raw json.RawMessage
		path := "/api/v1/export"
		if *keys {
			path += "?keys=1"
		}
		if err := a.call(http.MethodGet, path, nil, &raw); err != nil {
			return err
		}
		printJSON(raw)
	case "import":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := a.call(http.MethodPost, "/api/v1/import", json.RawMessage(data), &raw); err != nil {
			return err
		}
		printJSON(raw)
	default:
		return usageError(fmt.Sprintf("unknown command %q (opence-fleet help)", command))
	}
	return nil
}

// job starts a bulk action and follows it to its end.
func (a *api) job(action string, slugs []string, group string, params map[string]any, w *tabwriter.Writer) error {
	var answer struct {
		Job int64 `json:"job"`
	}
	params["action"] = action
	var err error
	if group != "" {
		err = a.call(http.MethodPost, "/api/v1/groups/"+url.PathEscape(group)+"/"+action, params, &answer)
	} else {
		params["slugs"] = slugs
		err = a.call(http.MethodPost, "/api/v1/jobs", params, &answer)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "job %d runs\n", answer.Job)
	for {
		var state struct {
			Job struct {
				State  string `json:"State"`
				Detail string `json:"Detail"`
			} `json:"job"`
			Items []struct {
				Slug   string `json:"Slug"`
				State  string `json:"State"`
				Detail string `json:"Detail"`
			} `json:"items"`
		}
		if err := a.call(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", answer.Job), nil, &state); err != nil {
			return err
		}
		if state.Job.State != "running" && state.Job.State != "queued" {
			fmt.Fprintln(w, "SERVER\tSTATE\tDETAIL")
			for _, it := range state.Items {
				fmt.Fprintf(w, "%s\t%s\t%s\n", it.Slug, it.State, it.Detail)
			}
			w.Flush()
			if state.Job.State != "done" {
				return fmt.Errorf("the job is %s: %s", state.Job.State, state.Job.Detail)
			}
			return nil
		}
		time.Sleep(2 * time.Second)
	}
}
