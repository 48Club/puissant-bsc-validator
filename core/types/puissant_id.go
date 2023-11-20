package types

import (
	"bytes"
	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/google/uuid"
	"math"
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

func GenPuissantID(txs []*Transaction, revertible mapset.Set[common.Hash], maxTimestamp uint64) PuissantID {
	var msg bytes.Buffer
	msg.Grow(len(txs)*(common.HashLength+1) + 4)

	for _, tx := range txs {
		msg.Write(tx.Hash().Bytes())
		if revertible.Contains(tx.Hash()) {
			msg.WriteByte(math.MaxUint8)
		} else {
			msg.WriteByte(0)
		}
	}

	msg.WriteByte(byte(maxTimestamp))
	msg.WriteByte(byte(maxTimestamp >> 8))
	msg.WriteByte(byte(maxTimestamp >> 16))
	msg.WriteByte(byte(maxTimestamp >> 24))

	return PuissantID(uuid.NewMD5(uuid.NameSpaceDNS, msg.Bytes()))
}
