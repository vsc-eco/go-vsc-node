package tss

type sessionInfo struct {
	leader string
	bh     uint64
}

// id = "vsc.tss_sign"
type CommitedSignedData struct {
	KeyId string
	Msg   []byte
	Sig   []byte
}

// id = "vsc.tss_gen"
type CommitedKeyGen struct {
}

// id = "vsc.tss_reshare"
type CommitedReshare struct {
}

// id = "vsc.tss_blame"
type CommitedBlame struct {
}

type sigMsg struct {
	Account   string `json:"account"`
	Sig       string `json:"sig"`
	SessionId string `json:"session_id"`
}
