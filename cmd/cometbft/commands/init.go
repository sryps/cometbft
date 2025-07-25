package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	cfg "github.com/cometbft/cometbft/v2/config"
	"github.com/cometbft/cometbft/v2/crypto/ed25519"
	kt "github.com/cometbft/cometbft/v2/internal/keytypes"
	cmtos "github.com/cometbft/cometbft/v2/internal/os"
	cmtrand "github.com/cometbft/cometbft/v2/internal/rand"
	"github.com/cometbft/cometbft/v2/p2p"
	"github.com/cometbft/cometbft/v2/privval"
	"github.com/cometbft/cometbft/v2/types"
	cmttime "github.com/cometbft/cometbft/v2/types/time"
)

// InitFilesCmd initializes a fresh CometBFT instance.
var InitFilesCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize CometBFT",
	RunE:  initFiles,
}

func init() {
	InitFilesCmd.Flags().StringVarP(&keyType, "key-type", "k", ed25519.KeyType, fmt.Sprintf("private key type (one of %s)", kt.SupportedKeyTypesStr()))
}

func initFiles(*cobra.Command, []string) error {
	return initFilesWithConfig(config)
}

func initFilesWithConfig(config *cfg.Config) error {
	// private validator
	privValKeyFile := config.PrivValidatorKeyFile()
	privValStateFile := config.PrivValidatorStateFile()
	var pv *privval.FilePV
	if cmtos.FileExists(privValKeyFile) {
		pv = privval.LoadFilePV(privValKeyFile, privValStateFile)
		logger.Info("Found private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile)
	} else {
		var err error
		pv, err = privval.GenFilePV(privValKeyFile, privValStateFile, genPrivKeyFromFlag)
		if err != nil {
			return fmt.Errorf("can't generate file pv: %w", err)
		}
		pv.Save()
		logger.Info("Generated private validator", "keyFile", privValKeyFile,
			"stateFile", privValStateFile)
	}

	nodeKeyFile := config.NodeKeyFile()
	if cmtos.FileExists(nodeKeyFile) {
		logger.Info("Found node key", "path", nodeKeyFile)
	} else {
		if _, err := p2p.LoadOrGenNodeKey(nodeKeyFile); err != nil {
			return err
		}
		logger.Info("Generated node key", "path", nodeKeyFile)
	}

	// genesis file
	genFile := config.GenesisFile()
	if cmtos.FileExists(genFile) {
		logger.Info("Found genesis file", "path", genFile)
	} else {
		genDoc := types.GenesisDoc{
			ChainID:         fmt.Sprintf("test-chain-%v", cmtrand.Str(6)),
			GenesisTime:     cmttime.Now(),
			ConsensusParams: types.DefaultConsensusParams(),
		}
		pubKey, err := pv.GetPubKey()
		if err != nil {
			return fmt.Errorf("can't get pubkey: %w", err)
		}
		genDoc.Validators = []types.GenesisValidator{{
			Address: pubKey.Address(),
			PubKey:  pubKey,
			Power:   10,
		}}

		if err := genDoc.SaveAs(genFile); err != nil {
			return err
		}
		logger.Info("Generated genesis file", "path", genFile)
	}

	return nil
}
