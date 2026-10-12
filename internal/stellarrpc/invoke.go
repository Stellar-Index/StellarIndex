package stellarrpc

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// InvokeContractTxEnvelope builds an unsigned base64 TransactionEnvelope for [Client.SimulateTransaction] from a C-strkey contractID, function Symbol and SCVal args; the source account can be any G-strkey and Fee/SeqNum are ignored placeholders, so it is not submittable.
func InvokeContractTxEnvelope(sourceAccount, contractID, functionName string, args []xdr.ScVal) (string, error) {
	if sourceAccount == "" {
		// Any valid G-strkey works — simulation ignores the source.
		// Use a documented "null" account: 32 zero bytes encoded as
		// a G-strkey. stellar-rpc accepts any well-formed strkey.
		var zero [32]byte
		enc, err := strkey.Encode(strkey.VersionByteAccountID, zero[:])
		if err != nil {
			return "", fmt.Errorf("stellarrpc: encode null source: %w", err)
		}
		sourceAccount = enc
	}

	muxed, err := parseSource(sourceAccount)
	if err != nil {
		return "", fmt.Errorf("stellarrpc: source %q: %w", sourceAccount, err)
	}

	ca, err := parseContractAddress(contractID)
	if err != nil {
		return "", fmt.Errorf("stellarrpc: contract %q: %w", contractID, err)
	}

	op := xdr.Operation{
		Body: xdr.OperationBody{
			Type: xdr.OperationTypeInvokeHostFunction,
			InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
				HostFunction: xdr.HostFunction{
					Type: xdr.HostFunctionTypeHostFunctionTypeInvokeContract,
					InvokeContract: &xdr.InvokeContractArgs{
						ContractAddress: ca,
						FunctionName:    xdr.ScSymbol(functionName),
						Args:            xdr.ScVec(args),
					},
				},
			},
		},
	}

	tx := xdr.Transaction{
		SourceAccount: muxed,
		Fee:           100, // placeholder; ignored by simulate
		SeqNum:        0,   // placeholder; ignored by simulate
		Operations:    []xdr.Operation{op},
	}

	env := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1:   &xdr.TransactionV1Envelope{Tx: tx}, // no signatures
	}

	b64, err := xdr.MarshalBase64(env)
	if err != nil {
		return "", fmt.Errorf("stellarrpc: marshal envelope: %w", err)
	}
	return b64, nil
}

// parseSource turns a G-strkey into the xdr.MuxedAccount shape
// TransactionEnvelope.Tx.SourceAccount expects.
func parseSource(strkeyAddr string) (xdr.MuxedAccount, error) {
	raw, err := strkey.Decode(strkey.VersionByteAccountID, strkeyAddr)
	if err != nil {
		return xdr.MuxedAccount{}, err
	}
	var ed xdr.Uint256
	copy(ed[:], raw)
	return xdr.MuxedAccount{
		Type:    xdr.CryptoKeyTypeKeyTypeEd25519,
		Ed25519: &ed,
	}, nil
}

// parseContractAddress turns a C-strkey into the xdr.ScAddress
// shape InvokeContractArgs.ContractAddress expects.
func parseContractAddress(strkeyAddr string) (xdr.ScAddress, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, strkeyAddr)
	if err != nil {
		return xdr.ScAddress{}, err
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	return xdr.ScAddress{
		Type:       xdr.ScAddressTypeScAddressTypeContract,
		ContractId: &cid,
	}, nil
}
