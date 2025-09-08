package sdk

import "strings"

type Intent struct {
	Type string            `json:"type"`
	Args map[string]string `json:"args"`
}

type Sender struct {
	Address              Address   `json:"id"`
	RequiredAuths        []Address `json:"required_auths"`
	RequiredPostingAuths []Address `json:"required_posting_auths"`
}

type AddressDomain string

const (
	AddressDomainUser     AddressDomain = "user"
	AddressDomainContract AddressDomain = "contract"
	AddressDomainSystem   AddressDomain = "system"
)

type AddressType string

const (
	AddressTypeEVM     AddressType = "evm"
	AddressTypeKey     AddressType = "key"
	AddressTypeHive    AddressType = "hive"
	AddressTypeSystem  AddressType = "system"
	AddressTypeBLS     AddressType = "bls"
	AddressTypeUnknown AddressType = "unknown"
)

type Address string

func (a Address) String() string {
	return string(a)
}

func (a Address) Domain() AddressDomain {
	if strings.HasPrefix(a.String(), "system:") {
		return AddressDomainSystem
	}
	if strings.HasPrefix(a.String(), "contract:") {
		return AddressDomainContract
	}
	return AddressDomainUser
}

func (a Address) Type() AddressType {
	if strings.HasPrefix(a.String(), "did:pkh:eip155") {
		return AddressTypeEVM
	} else if strings.HasPrefix(a.String(), "did:key:") {
		return AddressTypeKey
	} else if strings.HasPrefix(a.String(), "hive:") {
		return AddressTypeHive
	} else if strings.HasPrefix(a.String(), "system:") {
		return AddressTypeSystem
	} else {
		return AddressTypeUnknown
	}
	//TODO: Detect BLS address type, though it is not used or planned to be supported.
}

func (a Address) IsValid() bool {
	if a.Type() == AddressTypeUnknown {
		return false
	}
	return true
}
