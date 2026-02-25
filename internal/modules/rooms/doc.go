// Package rooms implements polymorphic rooms: PMs, Space rooms, threads.
package rooms

// Room types:
//   - PM (1): 1:1 private message
//   - GROUP_PM (2): Group private message
//   - SPACE_TEXT (3), SPACE_VOICE (4): Space rooms
//   - ROOM_SECTION (5): Room section (category for organizing Space rooms)
//   - THREAD (6): Thread under a parent room
//
// Real-time (Stargate): Clients subscribe with space_id = room_id to receive
// messages and events for that room. ROOM_CREATE is published when a new PM
// is created so the recipient sees it in real-time.
