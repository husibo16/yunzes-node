package format

// RuntimeKey returns the canonical runtime key used as inbound tag and limiter
// map key throughout the node:  coreType + "|" + logicalTag.
//
// coreType is "xray" or "sing"; logicalTag is the human-facing buildNodeTag
// output (e.g. "[host]-vless:1"). The pipe separator is reserved at this layer
// so downstream consumers can still construct user-keys via UserTag(runtimeKey,
// uuid) without ambiguity — they look up the whole produced string and never
// split it.
func RuntimeKey(coreType, logicalTag string) string {
	return coreType + "|" + logicalTag
}
