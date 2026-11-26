package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"strconv"
)

const definitionHashVersion = "dbmp-revision-v1"

func ComputeDefinitionHash(rev MigrationRevision) string {
	h := sha256.New()
	writeField(h, definitionHashVersion)
	writeField(h, rev.UpSQL)
	writeField(h, rev.DownSQL)
	writeField(h, strconv.FormatBool(rev.Reversible))
	if rev.Batch == nil {
		writeField(h, string(ExecutionModeNormal))
	} else {
		writeField(h, string(ExecutionModeBatch))
		writeField(h, rev.Batch.Table)
		writeField(h, rev.Batch.CursorColumn)
		writeField(h, strconv.Itoa(rev.Batch.BatchSize))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeField(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	h.Write(n[:])
	h.Write([]byte(s))
}
