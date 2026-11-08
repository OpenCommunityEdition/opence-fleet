# Anti-Cheat Audit and Host Security Model

This document specifies the security and authority architecture of the OpenCE dedicated server (network version 41). It documents every protection mechanism, whether authority resides on the host or client, the exact implementation locations in source code, verified operational limits, and instructions for server operators.

---

## Executive Summary

Halo CE's distributed network model in OpenCE is hybrid: **server-authoritative simulation with client-side prediction and lag-compensated hit registration**.

1. **Damage Authority**: Damage is dealt exclusively by the host (`network_damage.c:7-8`). Clients report candidate hits (`_distributed_message_hit_reports`). The host strictly audits each report against weapon ownership, firing rate limits, line-of-sight geometry, historical trajectory, bounding spheres, and time bounds (`network_damage.c:2198-2433`). Clients cannot inflict arbitrary damage or fabricate weapon damage types they do not possess.
2. **Weapons and Inventory Authority**: All inventories, carried weapons, total ammunition, and loaded rounds are completely authoritative on the host (`network_objects.c:1621-1658`). The host sends inventory states down to clients (`_distributed_message_inventories`); clients never dictate their ammunition or grenade counts to the host.
3. **Score Authority**: Kills, deaths, team scores, and game objectives are computed solely by the host (`network_distributed.c:10, 4985-5001`).
4. **Movement and Speed Validation**: Client movement is predicted locally, but the host validates unit speed, jump velocities, and flight ceilings each tick (`network_distributed.c:2211-2348, 2445-2543`). Clock manipulation (speed hacking) is monitored over sliding 2-second windows with automatic prediction refusal and stream-verified bans (`network_distributed.c:4594-4694`).

---

## Protection Matrix

| System / Vector | Authority Model | Host Enforcement Evidence | Vulnerability / Spoof Status |
| :--- | :--- | :--- | :--- |
| **Damage Execution** | Host Authoritative | `network_damage.c:7-8, 2472-2473` | **Enforced**. Clients cannot apply direct damage; only the host calls `object_cause_damage` in production (the test harness excepted, §1). |
| **Hit Report Validation** | Host Audited | `network_damage.c:2198-2433` | **Enforced**. Validates weapon held, rate of fire, distance reach, LoS raycast, target history, and time bounds. |
| **Damage Reach & Range** | Host Audited | `network_damage.c:996-1046, 2291-2297, 2339-2346` | **Enforced**. Projectile flight envelope and bounding radius checked against weapon tag limits. |
| **Line-of-Sight Check** | Host Audited | `network_damage.c:2111-2185, 2368-2385` | **Enforced**. World structure raycast (`collision_test_line`) prevents shooting through solid geometry. |
| **Weapon Firing Rates** | Host Audited | `network_damage.c:108-115, 2309-2314` | **Enforced**. Rate bucket (`HIT_REPORT_BURST_SECONDS`) caps hit frequency to tag trigger rates. |
| **Inventory & Ammo** | Host Authoritative | `network_objects.c:1621-1658, 3040-3181` | **Enforced**. Host simulates magazines and grenades; client takes host inventory. |
| **Scores & Statistics** | Host Authoritative | `network_distributed.c:10, 4985-5001` | **Enforced**. Score delta messages from clients do not exist. |
| **Speed Hack (Clock)** | Host Monitored | `network_distributed.c:4594-4694` | **Enforced**. Refuses predictions at >1.1x speed; bans at 10 s sustained stream-verified skew. |
| **Movement & Flying** | Host Clamped | `network_distributed.c:2211-2348, 2485-2516` | **Enforced**. Client positions clamped to tag biped speed, jump velocity, and fall rates. |
| **Teleportation** | Host Clamped | `network_distributed.c:2485` | **Enforced**. Delta positions > `HOST_ACCEPT_TOLERANCE` (3.5 world units) rejected. |
| **Bans & Identity** | Host Enforced | `network_distributed.c:4455-4523` | **Enforced**. IP, hardware ID and verified Discord account checked against `bans.txt` on connection. |
| **Vote-Kick Integrity** | Host Stream-Gated | `network_distributed.c:4944-4945`, `network_votekick.c:1015-1023` | **Enforced**. Only accepted from reliable stream; counted by unique IP and HWID. |
| **Aimbot / Memory Injection** | Client Memory | N/A (Client Execution) | **Not Protected by Host**. Host cannot detect external memory manipulation or aim snapping. |

---

## Detailed Audit Findings

### 1. Damage Reach and Hit Report Validation

#### Test Harness vs Production Path
The distance checks in `port/linux/game/network_test.c:568-580` (`network_test_shoot`) are exclusive to the debug harness (`debug.network_test_shoot` / `HALO_NETWORK_TEST_SHOOT`) used by `tools/net_compare.py`. They never execute during normal server hosting.

However, the production damage pipeline in `port/linux/game/network_damage.c` implements comprehensive, server-side validation:
1. **Report Origin and Ownership** (`network_damage.c:2222-2227`): Host verifies that the reporting machine owns the player claimed as the damage dealer.
2. **Weapon Ownership Verification** (`network_damage.c:2254-2258`, `1330-1417`, `1861-1932`): Host inspects the player's recent weapon history (`damage_players[player].definition_indices`) within `RECENT_WEAPON_TICKS` (10 seconds / 300 ticks). If the damage effect tag does not match a weapon or projectile currently or recently held by the player, the report is rejected (`rate <= 0.0f`).
3. **Grenade Consumption** (`network_damage.c:2302-2308, 2315-2317`): Grenade damage is accepted only if the host witnessed the player throw a grenade (`distributed_grenade_throw`) of that type within the valid time window. Once detonated, the recorded throw is consumed (`damage_players[player].throw_times[throw_index] = NONE`).
4. **Burst Rate Limiting** (`network_damage.c:2309-2314`): Weapon hit reports are metered by `distributed_report_paid` against the weapon trigger's rate of fire with a 3-second burst tolerance (`HIT_REPORT_BURST_SECONDS = 3`, `HIT_REPORT_RATE_MARGIN = 2`). Flooding hit reports beyond tag firing parameters fails validation.
5. **Melee Distance and Origin** (`network_damage.c:2318-2325`, `1757-1785`): Melee damage requires the host's historical trail of the player's unit position (`distributed_player_was_near`) within the target history buffer to be in proximity to both the impact origin and the epicenter.
6. **Vehicle Collision Verification** (`network_damage.c:2334-2338`, `1786-1826`): Damage from vehicle impact requires the host to confirm that the reporting player was driving the vehicle and that the vehicle trajectory intercepted the target within bounding sphere radii.
7. **Projectile Flight Reach** (`network_damage.c:2339-2346`, `996-1046`): Point damage (bullets, projectiles) must lie within the reach distance (`damage_reach.distance`) calculated from tag velocity and maximum lifetime (`distributed_projectile_flight`), measured from anywhere the player has been along the historical movement trail (`damage_trails`).
8. **Target Historical Window (Lag Compensation)** (`network_damage.c:2347-2367`): Target position reported by the client is verified against the host's object history buffer (`damage_history`, 32 ticks / ~1.07 s). If the target was not near that position within `REPORT_MAXIMUM_AGE_TICKS` (90 ticks / 3.0 s), the report is rejected.
9. **Line-of-Sight Raycasting** (`network_damage.c:2368-2385`, `2111-2185`): Before applying point damage, the host traces collision rays (`collision_test_line` with `_collision_test_structure_bit`) between the shooter's historical eye positions and the target bounding sphere. If solid world geometry occludes all line-of-sight paths, the damage is discarded.

### 2. Ammunition and Grenade Authority

- **Inventory Transmission**: The host packages unit inventory state in `distributed_inventory_from_unit` (`network_objects.c:1621-1658`) and transmits it via `_distributed_message_inventories` (`network_objects.c:1802-1813`).
- **Client Handling**: Clients receive inventory packets in `network_objects_handle_inventories` (`network_objects.c:3262-3271`) and overwrite local magazine and grenade state via `distributed_client_apply_inventory` (`network_objects.c:3040-3181`).
- **No Client-to-Host Inventory Packet**: In `network_distributed.c:4860-4887, 5012-5014`, `_distributed_message_inventories` is processed exclusively by clients (`game_connection() == _game_connection_network_client`). If a rogue client sends an inventory message to the host, it is ignored and dropped.
- **Weapon Firing**: Weapon firing is simulated on the host by feeding player input actions (`update_server_handle_distributed_input`, `network_distributed.c:2866`). Ammo is deducted server-side by weapon simulation routines.

### 3. Score and Objective Authority

- **Host Simulation**: Kill attribution and deaths are registered by the host in `network_damage.c` and recorded in `distributed_deaths` (`network_distributed.c:3362-3429`).
- **Statistics Broadcast**: Every 15 ticks (0.5 seconds) and whenever statistics change, the host broadcasts `_distributed_message_player_statistics` (`network_distributed.c:3609-3692, 3876-3879`).
- **Client Reception**: Clients update scoreboard numbers in `network_distributed.c:4985-5001`. Clients have no message type to report scores, kills, or objective captures to the host.

### 4. Movement and Speed Hack Detection

#### Speed Hack Detection Engine
Implemented in `network_distributed.c:4594-4694` (`distributed_note_client_clock`):
- **Measurement Window**: Measured over 2000 ms sliding windows (`CLIENT_CLOCK_WINDOW_MILLISECONDS = 2000`, `network_distributed.c:137`).
- **Rate Threshold**: Clock rate exceeding 1.10x real time (`CLIENT_CLOCK_FAST_RATE = 1.1f`, `network_distributed.c:141`).
- **Ahead Threshold**: Client clock more than 15 ticks (0.50 s) ahead of host time (`CLIENT_CLOCK_AHEAD_TICKS = 15`, `network_distributed.c:142`).
- **Immediate Penalty**: When both conditions are met, the machine is flagged `clock->fast = TRUE`. During this time, `distributed_machine_clock_fast(machine_index)` causes `network_distributed.c:4911-4917` to immediately drop all incoming `_distributed_message_player_prediction` and `_distributed_message_vehicle_prediction` packets. The host simulates the player using server-side ticks only.
- **Auto-Kick / Ban Duration**: 5 consecutive fast windows (`CLIENT_CLOCK_FAST_WINDOWS = 5`, 10 seconds total).
- **Stream Confirmation**: To prevent UDP source IP spoofing from maliciously disconnecting other players, the host requires confirmation on the reliable TCP/TLS stream (`clock->ahead_on_stream = TRUE`, `network_distributed.c:4605-4606, 4624`). If fast clock ticks are observed only in UDP datagrams, the player is not dropped; an unverified warning is logged to `cheaters.txt` (`network_distributed.c:4670-4690`).

#### Movement Validation Bounds
Implemented in `network_distributed.c:2211-2348` (`distributed_on_foot_bound`) and `2445-2543` (`distributed_take_prediction`):
- **Maximum Step Distance**: Target position delta between ticks must not exceed `HOST_ACCEPT_TOLERANCE` (3.5 world units, `network_distributed.c:2485`; constant at `network_distributed.c:290`).
- **Horizontal & Vertical Velocity**: Velocity is bounded by `run_speed`, `jump_speed`, and double-speed traits (`network_distributed.c:2272-2288`).
- **Ceiling Clamping**: Z-height is restricted by `distributed_on_foot_ceiling` (`network_distributed.c:2512-2516`). Floating or flying above legitimate jump and explosive knockback arcs is rejected.

---

## Operator Guide: Bans, Cheater Logs, and Recovery

### File Locations on Linux

The server engine references files using Xbox drive paths:
- `d:\bans.txt`
- `d:\cheaters.txt`

Under the Linux platform abstraction layer (`port/linux/src/xbox_files.c:6-17` and `port/linux/src/msvc_crt.c:576-581`), `d:\` translates to the **data root**:
1. Directory path configured by `paths.data` in `config.toml`.
2. Or current working directory if it contains a `maps/` folder.
3. In the official Docker container and server deployment layout, these files are written to the server runtime directory:
   - `/root/halo-server-pkg/server/bans.txt` (or `/app/server/bans.txt` in container volume).
   - `/root/halo-server-pkg/server/cheaters.txt`.

### File Format

Entries are written as single-line ASCII records (`network_distributed.c:4348-4398`):
```text
YYYY-MM-DD HH:MM:SS	ip=<ip_address>	hwid=<32_hex_chars>	discord_username=<username>	discord_id=<discord_id>	players=<player_names>	reason=<reason>
```
A Discord name or id the machine reported about itself is followed by ` (self-reported)`; a temporary ban adds a trailing `until=<unix_seconds>` field.

Example:
```text
2026-10-24 21:05:12	ip=198.51.100.42	hwid=a1b2c3d4e5f60718293a4b5c6d7e8f90	discord_username=spartan	discord_id=123456789012345678	players=MasterChief	reason=speed hack (game ran 1.45 times as fast)
```

### Unbanning a Player

The ban list is checked upon connection in `network_distributed_banned` (`network_distributed.c:4455-4523`):
1. Open `bans.txt` in any text editor.
2. Locate the line containing the target IP, HWID, or verified Discord id.
3. Delete the line and save the file.
4. Changes take effect on subsequent connection attempts (the file is re-read on every incoming connection handshake).

### Diagnosing Network Lag and Clock False Positives

Players on unstable links, cellular connections, or behind Carrier-Grade NAT (CGNAT) may experience sudden burst packet delivery.
- **Why False Positives are Rare**: A delayed burst may cause ticks to arrive quickly, but `clock->latest_tick - game_time_get()` will not remain sustained ahead of the host clock for 10 continuous seconds. Furthermore, unverified UDP packet bursts alone never trigger an auto-kick (`network_distributed.c:4665-4690`).
- **Symptom of Real Speed Hack**: Consistent logging in `debug.txt` showing `machine #N's game runs X.XX times as fast as this host's` across 5 consecutive windows (10 s), accompanied by stream verification.
- **Symptom of Network Jitter / Packet Delivery Burst**: An isolated log entry `datagrams said its game ran X times as fast... its stream not: not dropped (unverified)`. If an operator sees unverified warnings without actual speed hack kicks, the player is experiencing bufferbloat or connection reordering, not running a cheat.

---

## Known Gaps and Unprotected Vectors

The following vectors are not protected by the dedicated server engine:

| Vector | Severity | Explanation | Mitigation |
| :--- | :--- | :--- | :--- |
| **Client-Side Aimbot / Auto-Aim** | **High** | Clients compute their own input pitch and yaw angles locally. Memory injection or aim assistance tools that lock onto player bounding boxes cannot be detected server-side. | Operator observation, player vote-kick (`votekick <player>`), and community reports. |
| **Wallhack / ESP (Entity Display)** | **High** | OpenCE sends player positions within network visibility zones to all clients for smooth local interpolation. Client memory scanners can read unit coordinates and render ESP overlays. | Fast kick/ban via operator console; player vote-kicking. |
| **HWID Spoofing** | **Medium** | The client generates its hardware ID locally (`p2p_hardware_id`). A malicious actor running with administrative privileges can modify `/etc/machine-id` or spoof SMBIOS data to generate a new HWID. | Combined IP banning + server password gating (`network.server_password`). |
| **Public Chat Forced Off Bug** | **Low** | In public listed games, text chat was historically forced off in `chat_host_config_flags`. | Resolved in Milestone 2 (`network.public_chat = true`). |

---

## Conclusion

The OpenCE dedicated server engine exhibits rigorous server authority across all gameplay-critical states: hits are raycast-verified and history-bounded, inventories and ammunition are completely host-simulated, scores cannot be modified by clients, and clock manipulations trigger automated prediction revocation and kicking.

No engine code changes are required for anti-cheat verification in Milestone 4.
