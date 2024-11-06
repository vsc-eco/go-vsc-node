/*
This is an auto-generated file. DO NOT EDIT!
To modify generation, update /scripts/manager_gen/manager_gen.go
To modify protocol interface, update /modules/protocol/protocol.go
*/

package manager

import result "github.com/JustinKnueppel/go-result"

func (m *manager) ProposeBlock(id string) result.Result[any] {
	for i := m.version; i >= 0; i-- {
		protocol := m.protocols[i]
		if protocol.ProposeBlock != nil {
			return protocol.ProposeBlock(id)
		}
	}
	var res result.Result[any]
	return res
}
func (m *manager) SignMultiSig() {
	for i := m.version; i >= 0; i-- {
		protocol := m.protocols[i]
		if protocol.SignMultiSig != nil {
			protocol.SignMultiSig()
			return
		}
	}
	return
}
