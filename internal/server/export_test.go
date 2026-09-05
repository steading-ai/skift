package server

// ConnectError is the error mapping, for specs that check codes without a
// database.
var ConnectError = connectError

// Page tokens and enum tables, for the wire mapping specs.
var (
	EncodePageToken = encodePageToken
	DecodePageToken = decodePageToken
	NextPageToken   = nextPageToken
	TextDigest      = textDigest
)
