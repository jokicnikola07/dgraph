// Copyright 2019 ChainSafe Systems (ON) Corp.
// This file is part of gossamer.
//
// The gossamer library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gossamer library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gossamer library. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode"

	"github.com/ChainSafe/gossamer/dot"
	"github.com/ChainSafe/gossamer/dot/core"
	"github.com/ChainSafe/gossamer/dot/network"
	"github.com/ChainSafe/gossamer/dot/rpc"
	"github.com/ChainSafe/gossamer/dot/rpc/json2"
	"github.com/ChainSafe/gossamer/dot/state"
	"github.com/ChainSafe/gossamer/lib/common"
	"github.com/ChainSafe/gossamer/lib/genesis"
	"github.com/ChainSafe/gossamer/lib/keystore"
	"github.com/ChainSafe/gossamer/lib/runtime"
	"github.com/ChainSafe/gossamer/lib/services"
	"github.com/ChainSafe/gossamer/node/gssmr"

	log "github.com/ChainSafe/log15"
	"github.com/naoina/toml"
	"github.com/urfave/cli"
)

// makeNode sets up node; opening badgerDB instance and returning the Node container
func makeNode(ctx *cli.Context) (*dot.Node, *dot.Config, error) {
	currentConfig, err := getConfig(ctx)
	if err != nil {
		return nil, nil, err
	}

	log.Info("🕸\t Configuring node...", "datadir", currentConfig.Global.DataDir, "protocol", currentConfig.Network.ProtocolID, "bootnodes", currentConfig.Network.Bootnodes)

	var srvcs []services.Service

	dataDir := currentConfig.Global.DataDir

	// Create service, initialize stateDB and blockDB
	stateSrv := state.NewService(dataDir)
	srvcs = append(srvcs, stateSrv)

	err = stateSrv.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot start db service: %s", err)
	}

	// load all static keys from keystore directory
	ks := keystore.NewKeystore()
	// unlock keys, if specified
	if keyindices := ctx.String(UnlockFlag.Name); keyindices != "" {
		err = unlockKeys(ctx, dataDir, ks)
		if err != nil {
			return nil, nil, fmt.Errorf("could not unlock keys: %s", err)
		}
	}

	// Trie, runtime: load most recent state from DB, load runtime code from trie and create runtime executor
	rt, err := loadStateAndRuntime(stateSrv.Storage, ks)
	if err != nil {
		return nil, nil, fmt.Errorf("error loading state and runtime: %s", err)
	}

	// load genesis from JSON file
	gendata, err := stateSrv.Storage.LoadGenesisData()
	if err != nil {
		return nil, nil, err
	}

	// TODO: Configure node based on Roles #601

	// Network
	networkSrvc, networkMsgSend, networkMsgRec := createNetworkService(currentConfig, gendata, stateSrv)
	srvcs = append(srvcs, networkSrvc)

	// BABE authority configuration; flag overwrites config option
	if auth := ctx.GlobalBool(AuthorityFlag.Name); auth && !currentConfig.Global.Authority {
		currentConfig.Global.Authority = true
	} else if ctx.IsSet(AuthorityFlag.Name) && !auth && currentConfig.Global.Authority {
		currentConfig.Global.Authority = false
	}

	log.Info("node", "authority", currentConfig.Global.Authority)

	// Core
	coreConfig := &core.Config{
		BlockState:       stateSrv.Block,
		StorageState:     stateSrv.Storage,
		TransactionQueue: stateSrv.TransactionQueue,
		Keystore:         ks,
		Runtime:          rt,
		MsgRec:           networkMsgSend, // message channel from network service to core service
		MsgSend:          networkMsgRec,  // message channel from core service to network service
		IsBabeAuthority:  currentConfig.Global.Authority,
	}
	coreSrvc := createCoreService(coreConfig)
	srvcs = append(srvcs, coreSrvc)

	// RPC
	if ctx.GlobalBool(RPCEnabledFlag.Name) {
		rpcSrvr := setupRPC(currentConfig.RPC, stateSrv, networkSrvc, coreSrvc, stateSrv.TransactionQueue)
		srvcs = append(srvcs, rpcSrvr)
	}

	return dot.NewNode(gendata.Name, srvcs), currentConfig, nil
}

func loadStateAndRuntime(ss *state.StorageState, ks *keystore.Keystore) (*runtime.Runtime, error) {
	latestState, err := ss.LoadHash()
	if err != nil {
		return nil, fmt.Errorf("cannot load latest state root hash: %s", err)
	}

	err = ss.LoadFromDB(latestState)
	if err != nil {
		return nil, fmt.Errorf("cannot load latest state: %s", err)
	}

	code, err := ss.GetStorage([]byte(":code"))
	if err != nil {
		return nil, fmt.Errorf("error retrieving :code from trie: %s", err)
	}

	return runtime.NewRuntime(code, ss, ks)
}

// getConfig checks for config.toml if --config flag is specified and sets CLI flags
func getConfig(ctx *cli.Context) (*dot.Config, error) {
	currentConfig := gssmr.DefaultConfig()

	// Load config file.
	if file := ctx.GlobalString(ConfigFileFlag.Name); file != "" {
		configFile := ctx.GlobalString(ConfigFileFlag.Name)
		err := loadConfig(configFile, currentConfig)
		if err != nil {
			log.Warn("err loading toml file", "err", err.Error())
			return currentConfig, err
		}
	} else {
		log.Debug("Config File is not set")
	}

	//expand tilde or dot
	newDataDir := expandTildeOrDot(currentConfig.Global.DataDir)
	currentConfig.Global.DataDir = newDataDir

	// Parse CLI flags
	setGlobalConfig(ctx, &currentConfig.Global)
	setNetworkConfig(ctx, &currentConfig.Network)
	setRPCConfig(ctx, &currentConfig.RPC)
	return currentConfig, nil
}

// loadConfig loads the contents from config toml and inits Config object
func loadConfig(file string, config *dot.Config) error {
	fp, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	log.Debug("Loading configuration", "path", filepath.Clean(fp))
	f, err := os.Open(filepath.Clean(fp))
	if err != nil {
		return err
	}
	if err = tomlSettings.NewDecoder(f).Decode(&config); err != nil {
		return err
	}
	return nil
}

func setGlobalConfig(ctx *cli.Context, currentConfig *dot.GlobalConfig) {
	newDataDir := currentConfig.DataDir
	if dir := ctx.GlobalString(DataDirFlag.Name); dir != "" {
		newDataDir = expandTildeOrDot(dir)
	}
	currentConfig.DataDir, _ = filepath.Abs(newDataDir)

	newRoles := currentConfig.Roles
	if roles := ctx.GlobalString(RolesFlag.Name); roles != "" {
		b, err := strconv.Atoi(roles)
		if err != nil {
			log.Debug("Failed to convert to byte", "roles", roles)
		} else {
			newRoles = byte(b)
		}
	}
	currentConfig.Roles = newRoles
}

func setNetworkConfig(ctx *cli.Context, fig *dot.NetworkConfig) {
	// Bootnodes
	if bnodes := ctx.GlobalString(BootnodesFlag.Name); bnodes != "" {
		fig.Bootnodes = strings.Split(ctx.GlobalString(BootnodesFlag.Name), ",")
	}

	if protocol := ctx.GlobalString(ProtocolIDFlag.Name); protocol != "" {
		fig.ProtocolID = protocol
	}

	if port := ctx.GlobalUint(PortFlag.Name); port != 0 {
		fig.Port = uint32(port)
	}

	// NoBootstrap
	if off := ctx.GlobalBool(NoBootstrapFlag.Name); off {
		fig.NoBootstrap = true
	}

	// NoMdns
	if off := ctx.GlobalBool(NoMdnsFlag.Name); off {
		fig.NoMdns = true
	}
}

// createNetworkService creates a network service from the command configuration and genesis data
func createNetworkService(fig *dot.Config, gendata *genesis.Data, stateService *state.Service) (*network.Service, chan network.Message, chan network.Message) {
	// Default bootnodes and protocol from genesis file
	bootnodes := common.BytesToStringArray(gendata.Bootnodes)
	protocolID := gendata.ProtocolID

	// If bootnodes flag has one or more bootnodes, overwrite genesis bootnodes
	if len(fig.Network.Bootnodes) > 0 {
		bootnodes = fig.Network.Bootnodes
	}

	// If protocol id flag is not an empty string, overwrite
	if fig.Network.ProtocolID != "" {
		protocolID = fig.Network.ProtocolID
	}

	// network service configuation
	networkConfig := network.Config{
		BlockState:   stateService.Block,
		NetworkState: stateService.Network,
		DataDir:      fig.Global.DataDir,
		Roles:        fig.Global.Roles,
		Port:         fig.Network.Port,
		Bootnodes:    bootnodes,
		ProtocolID:   protocolID,
		NoBootstrap:  fig.Network.NoBootstrap,
		NoMdns:       fig.Network.NoMdns,
	}

	networkMsgRec := make(chan network.Message)
	networkMsgSend := make(chan network.Message)

	networkService, err := network.NewService(&networkConfig, networkMsgSend, networkMsgRec)
	if err != nil {
		log.Error("Failed to create new network service", "err", err)
	}

	return networkService, networkMsgSend, networkMsgRec
}

// createCoreService creates the core service from the provided core configuration
func createCoreService(coreConfig *core.Config) *core.Service {
	coreService, err := core.NewService(coreConfig)
	if err != nil {
		log.Crit("Failed to create new core service", "err", err)
		os.Exit(0)
	}

	return coreService
}

func setRPCConfig(ctx *cli.Context, fig *dot.RPCConfig) {
	// Modules
	if mods := ctx.GlobalString(RPCModuleFlag.Name); mods != "" {
		fig.Modules = strings.Split(ctx.GlobalString(RPCModuleFlag.Name), ",")
	}

	// Host
	if host := ctx.GlobalString(RPCHostFlag.Name); host != "" {
		fig.Host = host
	}

	// Port
	if port := ctx.GlobalUint(RPCPortFlag.Name); port != 0 {
		fig.Port = uint32(port)
	}

}

func setupRPC(fig dot.RPCConfig, stateSrv *state.Service, networkSrvc *network.Service, coreSrvc *core.Service, txQueue *state.TransactionQueue) *rpc.HTTPServer {
	cfg := &rpc.HTTPServerConfig{
		BlockAPI:            stateSrv.Block,
		StorageAPI:          stateSrv.Storage,
		NetworkAPI:          networkSrvc,
		CoreAPI:             coreSrvc,
		TransactionQueueAPI: txQueue,
		Codec:               &json2.Codec{},
		Host:                fig.Host,
		Port:                fig.Port,
		Modules:             fig.Modules,
	}

	return rpc.NewHTTPServer(cfg)
}

// dumpConfig is the dumpconfig command.
func dumpConfig(ctx *cli.Context) error {
	currentConfig, err := getConfig(ctx)
	if err != nil {
		return err
	}

	comment := ""

	out, err := toml.Marshal(currentConfig)
	if err != nil {
		return err
	}

	dump := os.Stdout
	if ctx.NArg() > 0 {
		/* #nosec */
		dump, err = os.OpenFile(filepath.Clean(ctx.Args().Get(0)), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}

		defer func() {
			err = dump.Close()
			if err != nil {
				log.Warn("err closing conn", "err", err.Error())
			}
		}()
	}
	_, err = dump.WriteString(comment)
	if err != nil {
		log.Warn("err writing comment output for dumpconfig command", "err", err.Error())
	}
	_, err = dump.Write(out)
	if err != nil {
		log.Warn("err writing comment output for dumpconfig command", "err", err.Error())
	}
	return nil
}

// These settings ensure that TOML keys use the same names as Go struct fields.
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string {
		return key
	},
	FieldToKey: func(rt reflect.Type, field string) string {
		return field
	},
	MissingField: func(rt reflect.Type, field string) error {
		link := ""
		if unicode.IsUpper(rune(rt.Name()[0])) && rt.PkgPath() != "main" {
			link = fmt.Sprintf(", see https://godoc.org/%s#%s for available fields", rt.PkgPath(), rt.Name())
		}
		return fmt.Errorf("field '%s' is not defined in %s%s", field, rt.String(), link)
	},
}

// expandTildeOrDot will expand a tilde prefix path to full home path
func expandTildeOrDot(targetPath string) string {
	if strings.HasPrefix(targetPath, "~\\") || strings.HasPrefix(targetPath, "~/") {
		if homeDir := gssmr.HomeDir(); homeDir != "" {
			targetPath = homeDir + targetPath[1:]
		}
	} else if strings.HasPrefix(targetPath, ".\\") || strings.HasPrefix(targetPath, "./") {
		targetPath, _ = filepath.Abs(targetPath)
	}
	return path.Clean(os.ExpandEnv(targetPath))
}