package transport

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"viper/internal/raft"
	"viper/pkg/contracts"
)

const maxPeerAddressBytes = 2048

// Peer is one immutable member of a statically configured cluster.
type Peer struct {
	ID         raft.NodeID
	Address    string
	ServerName string
}

// StaticCluster is a validated three- or five-node membership view. It does
// not expose mutation methods; membership changes require a new process
// configuration until joint consensus is implemented.
type StaticCluster struct {
	byID    map[raft.NodeID]Peer
	localID raft.NodeID
	ordered []Peer
}

// NewStaticCluster validates and freezes a local node's membership view.
func NewStaticCluster(localID raft.NodeID, peers []Peer) (*StaticCluster, error) {
	if len(peers) != 3 && len(peers) != 5 {
		return nil, invalidCluster("membership must contain exactly three or five nodes")
	}
	if err := contracts.NodeID(localID).Validate(); err != nil {
		return nil, invalidCluster("local node ID is invalid")
	}

	cluster := &StaticCluster{
		byID:    make(map[raft.NodeID]Peer, len(peers)),
		localID: localID,
		ordered: make([]Peer, 0, len(peers)),
	}
	addresses := make(map[string]raft.NodeID, len(peers))
	for _, candidate := range peers {
		peer, err := validatePeer(candidate)
		if err != nil {
			return nil, err
		}
		if _, exists := cluster.byID[peer.ID]; exists {
			return nil, invalidCluster(fmt.Sprintf("duplicate node ID %q", peer.ID))
		}
		if owner, exists := addresses[peer.Address]; exists {
			return nil, invalidCluster(fmt.Sprintf("nodes %q and %q share an address", owner, peer.ID))
		}
		cluster.byID[peer.ID] = peer
		addresses[peer.Address] = peer.ID
		cluster.ordered = append(cluster.ordered, peer)
	}
	if _, exists := cluster.byID[localID]; !exists {
		return nil, invalidCluster("local node is not a member")
	}
	sort.Slice(cluster.ordered, func(i, j int) bool {
		return cluster.ordered[i].ID < cluster.ordered[j].ID
	})
	return cluster, nil
}

func validatePeer(peer Peer) (Peer, error) {
	if err := contracts.NodeID(peer.ID).Validate(); err != nil {
		return Peer{}, invalidCluster("peer node ID is invalid")
	}
	peer.Address = strings.TrimSpace(peer.Address)
	peer.ServerName = strings.TrimSpace(peer.ServerName)
	if peer.Address == "" {
		return Peer{}, invalidCluster(fmt.Sprintf("peer %q has an empty address", peer.ID))
	}
	if len(peer.Address) > maxPeerAddressBytes {
		return Peer{}, invalidCluster(fmt.Sprintf("peer %q address is too long", peer.ID))
	}
	for _, character := range peer.Address {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return Peer{}, invalidCluster(fmt.Sprintf("peer %q address contains whitespace or control characters", peer.ID))
		}
	}
	if len(peer.ServerName) > contracts.MaxNodeIDBytes {
		return Peer{}, invalidCluster(fmt.Sprintf("peer %q server name is too long", peer.ID))
	}
	for _, character := range peer.ServerName {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return Peer{}, invalidCluster(fmt.Sprintf("peer %q server name contains whitespace or control characters", peer.ID))
		}
	}
	return peer, nil
}

// LocalID returns the node represented by this membership view.
func (c *StaticCluster) LocalID() raft.NodeID {
	if c == nil {
		return ""
	}
	return c.localID
}

// Quorum returns the number of members required for majority agreement.
func (c *StaticCluster) Quorum() int {
	if c == nil {
		return 0
	}
	return len(c.ordered)/2 + 1
}

// Contains reports whether an ID belongs to the immutable membership.
func (c *StaticCluster) Contains(id raft.NodeID) bool {
	if c == nil {
		return false
	}
	_, exists := c.byID[id]
	return exists
}

// Peer returns one configured member.
func (c *StaticCluster) Peer(id raft.NodeID) (Peer, bool) {
	if c == nil {
		return Peer{}, false
	}
	peer, exists := c.byID[id]
	return peer, exists
}

// Members returns a stable, isolated membership list sorted by node ID.
func (c *StaticCluster) Members() []Peer {
	if c == nil {
		return nil
	}
	return append([]Peer(nil), c.ordered...)
}

// RemotePeers returns all configured members except the local node.
func (c *StaticCluster) RemotePeers() []Peer {
	if c == nil {
		return nil
	}
	peers := make([]Peer, 0, len(c.ordered)-1)
	for _, peer := range c.ordered {
		if peer.ID != c.localID {
			peers = append(peers, peer)
		}
	}
	return peers
}
