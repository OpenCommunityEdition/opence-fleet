package update

// ReleaseKey is the public half of the fleet's release key (hex), which
// signs each release's SHA256SUMS (cmd/release-sign, in the release
// workflow). The updater installs nothing it does not sign, the agents'
// updates included; FLEET_UPDATE_KEY replaces it for a fork's releases.
const ReleaseKey = "73afba3fd73049c61352e343d5aaae29cd0f7bd3826c1760deeeb79c6cf9a474"
