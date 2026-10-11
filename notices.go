// Package mro2nf carries the license notices that must travel with every
// copy of mre and the Martian adapters, so the transpiler can write them into
// the container build contexts it generates.
package mro2nf

import _ "embed"

// License is mro2nf's own license.
//
//go:embed LICENSE
var License []byte

// ThirdPartyNotices is THIRD_PARTY_NOTICES (make notices).
//
//go:embed THIRD_PARTY_NOTICES
var ThirdPartyNotices []byte
