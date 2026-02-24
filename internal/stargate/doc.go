// Package stargate provides the WebSocket gateway for real-time messaging.
//
// Architecture
//
// Stargate is designed for multi-region scalability and federation:
//
//  1. Multi-region: Each region runs its own Stargate instance. Clients connect
//     to the nearest instance. Redis Pub/Sub relays messages across instances.
//
//  2. Redis Pub/Sub: When a client sends a message, the local instance publishes
//     to Redis. All instances (all regions) subscribe to the same channels.
//     Each instance fans out to its local subscribers only.
//
//  3. Federation: The message protocol includes origin/source fields for future
//     cross-deployment federation (e.g. strafe.chat <-> other.strafe.host).
//
// Channel model
//
//   - stargate:space:{space_id}  - Messages for a space (room/channel)
//   - stargate:user:{user_id}     - DMs, typing, presence
//   - stargate:presence:{space_id} - Optional presence in a space
//
// Auth: Session token via query param (?token=...) or cookie (session_token).
// Same session validation as REST API.
package stargate
