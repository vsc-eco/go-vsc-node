package messages

import (
	"vsc-node/modules/wasm/ipc_requests"
	"vsc-node/modules/wasm/ipc_requests/execute"
)

func MessageTypes[Result any]() map[string]ipc_requests.Message[Result] {
	return map[string]ipc_requests.Message[Result]{
		"sdk_call_request":  &execute.SdkCallRequest[Result]{},
		"sdk_call_response": &execute.SdkCallResponse[Result]{},
		"execution_finish":  &execute.ExecutionFinish[Result]{},
		"execution_ready":   &execute.ExecutionReady[Result]{},
		"execution_code":    &execute.ExecutionCode[Result]{},
	}
}
