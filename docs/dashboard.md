# Local dashboard contract

The dashboard is a read-only local observer of a fixed list of nodes. It does
not make cluster-wide availability, quorum, or leader guarantees. A node's
role and health are its own observations at the time of the request.

The dashboard listens only on an explicit loopback IP address. It serves its
page and status data from the same origin, validates the HTTP Host against that
listener, and does not enable CORS. It uses server-side, operator-specified
node addresses and TLS server names; browser requests cannot select targets.
The dashboard needs a client certificate authorized as an administrator by
each node: the current gRPC interceptor requires admin authorization for both
status scopes. Private keys and certificate paths never reach the browser.

For each node, query the existing liveness and readiness scopes with deadlines.
Liveness provides the node's reported status, role, term, commit index, and
applied index. Readiness is independently reported as successful or an RPC
failure: an unavailable readiness response alone does not establish why the
node is unavailable. Timeouts, authorization failures, and other failures are
safe categories, not raw error text. Failed requests do not prevent other nodes
from being observed. The response includes observation times; retained data
must be clearly marked stale after a failed refresh.

The dashboard has no KV operations, membership changes, backup controls, or
remote access. It does not change the protobuf API or Raft behavior. Remote
use requires a separate browser authentication and deployment design.
