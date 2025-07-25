package nodekey

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cometbft/cometbft/v2/crypto/ed25519"
	cmtrand "github.com/cometbft/cometbft/v2/internal/rand"
)

func TestLoadOrGen(t *testing.T) {
	filePath := filepath.Join(os.TempDir(), cmtrand.Str(12)+"_peer_id.json")

	nodeKey, err := LoadOrGen(filePath)
	require.NoError(t, err)

	nodeKey2, err := LoadOrGen(filePath)
	require.NoError(t, err)

	assert.Equal(t, nodeKey, nodeKey2)
}

func TestLoad(t *testing.T) {
	filePath := filepath.Join(os.TempDir(), cmtrand.Str(12)+"_peer_id.json")

	_, err := Load(filePath)
	assert.True(t, os.IsNotExist(err))

	_, err = LoadOrGen(filePath)
	require.NoError(t, err)

	nodeKey, err := Load(filePath)
	require.NoError(t, err)
	assert.NotNil(t, nodeKey)
}

func TestNodeKey_SaveAs(t *testing.T) {
	filePath := filepath.Join(os.TempDir(), cmtrand.Str(12)+"_peer_id.json")

	assert.NoFileExists(t, filePath)

	privKey := ed25519.GenPrivKey()
	nodeKey := &NodeKey{
		PrivKey: privKey,
	}
	err := nodeKey.SaveAs(filePath)
	require.NoError(t, err)
	assert.FileExists(t, filePath)
}

// ----------------------------------------------------------

func padBytes(bz []byte) []byte {
	targetBytes := 20
	return append(bz, bytes.Repeat([]byte{0xFF}, targetBytes-len(bz))...)
}

func TestPoWTarget(t *testing.T) {
	cases := []struct {
		difficulty uint
		target     []byte
	}{
		{0, padBytes([]byte{})},
		{1, padBytes([]byte{127})},
		{8, padBytes([]byte{0})},
		{9, padBytes([]byte{0, 127})},
		{10, padBytes([]byte{0, 63})},
		{16, padBytes([]byte{0, 0})},
		{17, padBytes([]byte{0, 0, 127})},
	}

	for _, c := range cases {
		assert.Equal(t, MakePoWTarget(c.difficulty, 20*8), c.target)
	}
}
