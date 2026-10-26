package core

import (
	"context"
	"strings"
	"testing"
)

// A profile's region is one of spec.Regions, or none: a typo, or a region
// of no list, is refused with the list.
func TestProfileRegion(t *testing.T) {
	f, _ := fleet(t)
	ctx := context.Background()
	for _, region := range []string{"eu", "NA-East", ""} {
		if _, err := f.CreateProfile(ctx, "user:paul", ProfileInput{Name: "P " + region, Region: region}); err != nil {
			t.Errorf("%q refused: %v", region, err)
		}
	}
	for _, region := range []string{"au", "europe", "eu-west"} {
		_, err := f.CreateProfile(ctx, "user:paul", ProfileInput{Name: "Q " + region, Region: region})
		if e, ok := AsError(err); !ok || e.Code != "schema" || !strings.Contains(e.Message, "eu, na-east, na-west") {
			t.Errorf("%q: %v", region, err)
		}
	}
}
