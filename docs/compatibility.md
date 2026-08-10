# Compatibility and upgrade boundary

Viper currently supports one compatibility version for the public protobuf
package, Raft WAL/snapshot formats, KV command/snapshot format, history files,
chaos schedules, and backup documents. Each format rejects unknown versions or
reserved flags before activation.

Rolling upgrades are supported only when the operator has verified that every
node understands the same format version and message-size limits. The current
implementation has no mixed-version compatibility matrix and therefore does
not claim rolling-upgrade compatibility. Upgrade or downgrade must be treated
as a maintenance event:

1. stop admission and take a verified backup;
2. retain the last-known-good store directory;
3. upgrade one isolated node and run `make verify`, recovery checks, and the
   local demo/test profile;
4. restore or roll back by activating a separate directory, never by editing
   WAL or snapshot bytes in place. The release binary reports its version,
   commit, and build date through `go run ./cmd/viper version` or the release
   binary's `version` command.

Dynamic membership and joint consensus are outside this compatibility
boundary. A peer-set edit requires a separately reviewed protocol and operator
workflow.
