package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/OpenCommunityEdition/opence-fleet/internal/agentproto"
	"github.com/OpenCommunityEdition/opence-fleet/internal/serverapi"
)

// watchExternal follows an external instance's status from the control
// plane (no agent runs it): every 5 seconds while it is in the database.
func (f *Fleet) watchExternal(ctx context.Context, slug string) {
	for {
		i, err := f.Instance(ctx, slug)
		if err != nil || i.Kind != "external" {
			return
		}
		password, _ := f.Key.OpenString(i.ExternalPasswordEnc, "instance:"+slug+":external_password")
		client := &serverapi.Client{Base: strings.TrimRight(i.ExternalUrl, "/"), Password: password, By: "opence-fleet", HTTP: f.HTTP}
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		status, answer, err := client.Do(call, "GET", "/api/status", nil)
		cancel()
		if err != nil || status != 200 {
			f.Live.SetInstance(i.NodeID, agentproto.Instance{Slug: slug, State: "unknown", Reason: "the server did not answer"})
		} else {
			f.Live.SetStatus(i.NodeID, slug, json.RawMessage(answer))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// WatchExternals starts following the external instances.
func (f *Fleet) WatchExternals(ctx context.Context) {
	instances, _ := f.Store.ListInstances(ctx)
	for _, i := range instances {
		if i.Kind == "external" {
			go f.watchExternal(ctx, i.Slug)
		}
	}
}
