package middleware_test

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	tmproto "github.com/tendermint/tendermint/proto/tendermint/types"
	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/simapp"
	"github.com/cosmos/cosmos-sdk/testutil/testdata"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	"github.com/cosmos/cosmos-sdk/x/auth/middleware"
	authsigning "github.com/cosmos/cosmos-sdk/x/auth/signing"
)

func TestRegisterMsgService(t *testing.T) {
	// Create an encoding config that doesn't register testdata Msg services.
	encCfg := simapp.MakeTestEncodingConfig()
	msr := middleware.NewMsgServiceRouter(encCfg.InterfaceRegistry)
	require.Panics(t, func() {
		testdata.RegisterMsgServer(
			msr,
			testdata.MsgServerImpl{},
		)
	})

	// Register testdata Msg services, and rerun `RegisterService`.
	testdata.RegisterInterfaces(encCfg.InterfaceRegistry)
	require.NotPanics(t, func() {
		testdata.RegisterMsgServer(
			msr,
			testdata.MsgServerImpl{},
		)
	})
}

func TestRegisterMsgServiceTwice(t *testing.T) {
	// Setup baseapp.
	encCfg := simapp.MakeTestEncodingConfig()
	msr := middleware.NewMsgServiceRouter(encCfg.InterfaceRegistry)
	testdata.RegisterInterfaces(encCfg.InterfaceRegistry)

	// First time registering service shouldn't panic.
	require.NotPanics(t, func() {
		testdata.RegisterMsgServer(
			msr,
			testdata.MsgServerImpl{},
		)
	})

	// Second time should panic.
	require.Panics(t, func() {
		testdata.RegisterMsgServer(
			msr,
			testdata.MsgServerImpl{},
		)
	})
}

func TestMsgService(t *testing.T) {
	app, _ := createTestApp(t, true)
	priv, _, addr := testdata.KeyTestPubAddr()
	encCfg := simapp.MakeTestEncodingConfig()
	testdata.RegisterInterfaces(encCfg.InterfaceRegistry)
	db := dbm.NewMemDB()
	baseApp := baseapp.NewBaseApp("test", log.NewTMLogger(log.NewSyncWriter(os.Stdout)), db, encCfg.TxConfig.TxDecoder())
	baseApp.SetInterfaceRegistry(encCfg.InterfaceRegistry)
	msr := middleware.NewMsgServiceRouter(encCfg.InterfaceRegistry)
	txHandler, err := middleware.NewDefaultTxHandler(middleware.TxHandlerOptions{
		MsgServiceRouter: msr,
		AccountKeeper:    app.AccountKeeper,
		BankKeeper:       app.BankKeeper,
		SignModeHandler:  encCfg.TxConfig.SignModeHandler(),
	})
	require.NoError(t, err)
	baseApp.SetTxHandler(txHandler)
	testdata.RegisterMsgServer(
		msr,
		testdata.MsgServerImpl{},
	)
	_ = baseApp.BeginBlock(abci.RequestBeginBlock{Header: tmproto.Header{Height: 1}})

	baseApp.MountStores(sdk.NewKVStoreKey("params"))
	err = baseApp.LoadLatestVersion()
	require.Nil(t, err)

	msg := testdata.TestMsg{Signers: []string{addr.String()}}
	txBuilder := encCfg.TxConfig.NewTxBuilder()
	txBuilder.SetFeeAmount(testdata.NewTestFeeAmount())
	txBuilder.SetGasLimit(testdata.NewTestGasLimit())
	err = txBuilder.SetMsgs(&msg)
	require.NoError(t, err)

	// First round: we gather all the signer infos. We use the "set empty
	// signature" hack to do that.
	sigV2 := signing.SignatureV2{
		PubKey: priv.PubKey(),
		Data: &signing.SingleSignatureData{
			SignMode:  encCfg.TxConfig.SignModeHandler().DefaultMode(),
			Signature: nil,
		},
		Sequence: 0,
	}

	err = txBuilder.SetSignatures(sigV2)
	require.NoError(t, err)

	// Second round: all signer infos are set, so each signer can sign.
	signerData := authsigning.SignerData{
		ChainID:       "test",
		AccountNumber: 0,
		Sequence:      0,
	}
	sigV2, err = tx.SignWithPrivKey(
		encCfg.TxConfig.SignModeHandler().DefaultMode(), signerData,
		txBuilder, priv, encCfg.TxConfig, 0)
	require.NoError(t, err)
	err = txBuilder.SetSignatures(sigV2)
	require.NoError(t, err)

	// Send the tx to the app
	txBytes, err := encCfg.TxConfig.TxEncoder()(txBuilder.GetTx())
	require.NoError(t, err)
	res := baseApp.DeliverTx(abci.RequestDeliverTx{Tx: txBytes})
	require.Equal(t, abci.CodeTypeOK, res.Code, "res=%+v", res)
}
