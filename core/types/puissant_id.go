package types

import (
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/google/uuid"
	"math/big"
	"strings"
)

const (
	PaymentGasUsageBaseLine = 21000
	PuissantStatusReportURL = "https://explorer.48.club/api/v1/puissant_update"
)

var (
	BigPaymentGasUsageBaseLine  = big.NewInt(PaymentGasUsageBaseLine)
	PuissantErrorInvalidPayment = errors.New("!21000")
	PuissantErrorTxNoRun        = errors.New("no run")
	PuissantErrorTxConflict     = errors.New("conflict")
)

type PuissantID [16]byte

func (p PuissantID) Bytes() []byte { return p[:] }

func (p PuissantID) Hex() string { return hexutil.Encode(p[:]) }

func (p PuissantID) String() string {
	return p.Hex()
}

func HexToPuissantID(s string) PuissantID { return BytesToPuissantID(common.FromHex(s)) }

func BytesToPuissantID(b []byte) PuissantID {
	var a PuissantID
	a.setBytes(b)
	return a
}

func (p PuissantID) IsPuissant() bool {
	return p != PuissantID{}
}

func (p PuissantID) setBytes(b []byte) {
	if len(b) > len(p) {
		b = b[len(b)-16:]
	}
	copy(p[16-len(b):], b)
}

// GenPuissantID generates a PuissantID from a list of transactions.
// Note! The transactions must set isAcceptReverting() before calling this function.
func GenPuissantID(txs []*Transaction) PuissantID {
	var msg strings.Builder
	msg.Grow(len(txs) * 33)
	for _, tx := range txs {
		msg.Write(tx.Hash().Bytes())
		if tx.AcceptsReverting() {
			msg.WriteByte(1)
		} else {
			msg.WriteByte(0)
		}
	}

	return PuissantID(uuid.NewMD5(uuid.NameSpaceDNS, []byte(msg.String())))
}
