package api

// Storage reads relayed through an agent (0.61, #63). A storage target that
// only the agents reach (a NAS on a customer's network) cannot be read by the
// console itself. To browse the files of a backup there, the console asks an
// agent that uses the storage to read the repository's objects for it: the
// agent opens the storage with the credentials in the request, reads, and
// sends the bytes back. The console decrypts and does the rest as with any
// other storage. Only reads exist; nothing is written or removed.
//
// The console sets PollResponse.Relay while it has reads waiting for the
// agent; the agent then asks PathRelayNext for them (a long poll) until it
// has been idle for a while, and answers each one at PathRelayReplyPrefix+ID.

const (
	PathRelayNext        = "/api/agent/relay/next"
	PathRelayReplyPrefix = "/api/agent/relay/reply/"
)

// Relay operations, the read side of backend.Backend.
const (
	RelayLoad      = "load"       // the whole object
	RelayLoadRange = "load-range" // Length bytes from Offset (negative: from the end)
	RelaySize      = "size"       // the size in bytes, as decimal text
	RelayList      = "list"       // the names below Name, as a JSON array
)

// Headers of a relay reply. Without them the body is the result.
const (
	RelayErrorHeader    = "X-BackupZit-Relay-Error"     // the read failed: the message
	RelayNotFoundHeader = "X-BackupZit-Relay-Not-Found" // "1": the object does not exist
)

// RelayRequest is one read for the agent.
type RelayRequest struct {
	ID string `json:"id"`
	// Repository holds the storage address and its credentials; the
	// repository password is never sent (the console decrypts).
	Repository Repository `json:"repository"`
	Op         string     `json:"op"`
	Name       string     `json:"name"`
	Offset     int64      `json:"offset,omitempty"`
	Length     int        `json:"length,omitempty"`
}

// RelayNextResponse lists the reads waiting for the agent; empty when none
// came during the wait.
type RelayNextResponse struct {
	Requests []RelayRequest `json:"requests"`
}
