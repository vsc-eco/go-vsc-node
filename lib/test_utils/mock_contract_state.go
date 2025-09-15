package test_utils

import (
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/contracts"
)

type MockContractStateDb struct {
	aggregate.Plugin
	Outputs []contracts.ContractOutput
}

func (m *MockContractStateDb) IngestOutput(inputArgs contracts.IngestOutputArgs) {
	// Convert IngestOutputArgs to ContractOutput
	output := contracts.ContractOutput{
		Id:          inputArgs.Id,
		ContractId:  inputArgs.ContractId,
		StateMerkle: inputArgs.StateMerkle,
		BlockHeight: inputArgs.AnchoredHeight,
		Metadata:    inputArgs.Metadata,
		Inputs:      inputArgs.Inputs,
		Results:     inputArgs.Results,
	}

	// Check if output already exists, if so update it, otherwise append
	for i, existingOutput := range m.Outputs {
		if existingOutput.Id == output.Id {
			m.Outputs[i] = output
			return
		}
	}

	// If not found, append new output
	m.Outputs = append(m.Outputs, output)
}

func (m *MockContractStateDb) GetLastOutput(contractId string, height uint64) (contracts.ContractOutput, error) {
	var lastOutput contracts.ContractOutput
	found := false

	for _, output := range m.Outputs {
		if output.ContractId == contractId && uint64(output.BlockHeight) <= height {
			if !found || output.BlockHeight > lastOutput.BlockHeight {
				lastOutput = output
				found = true
			}
		}
	}

	if !found {
		return contracts.ContractOutput{}, nil // Return empty output and nil error to match original implementation
	}

	return lastOutput, nil
}

func (m *MockContractStateDb) GetOutput(outputId string) *contracts.ContractOutput {
	for _, output := range m.Outputs {
		if output.Id == outputId {
			return &output
		}
	}
	return nil
}

func (m *MockContractStateDb) FindOutputs(id *string, input *string, contract *string, offset int, limit int) ([]contracts.ContractOutput, error) {
	var results []contracts.ContractOutput

	for _, output := range m.Outputs {
		match := true

		if id != nil && output.Id != *id {
			match = false
		}

		if input != nil {
			found := false
			for _, inputId := range output.Inputs {
				if inputId == *input {
					found = true
					break
				}
			}
			if !found {
				match = false
			}
		}

		if contract != nil && output.ContractId != *contract {
			match = false
		}

		if match {
			results = append(results, output)
		}
	}

	// Apply offset and limit
	if offset < len(results) {
		end := offset + limit
		if end > len(results) {
			end = len(results)
		}
		results = results[offset:end]
	} else {
		results = []contracts.ContractOutput{}
	}

	return results, nil
}
