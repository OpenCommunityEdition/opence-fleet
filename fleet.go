// Package opencefleet holds the files the binary embeds from the
// repository's root: the node installer, which the control plane serves at
// /agent.sh.
package opencefleet

import _ "embed"

// AgentScript is agent.sh, with its @FLEET_URL@, @FLEET_VERSION@,
// @RELEASE_KEY@, @SELF_ARCH@ and @SELF_SHA256@ filled in when served.
//
//go:embed agent.sh
var AgentScript []byte
