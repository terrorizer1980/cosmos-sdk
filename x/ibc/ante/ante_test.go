package ante_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	abci "github.com/tendermint/tendermint/abci/types"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/simapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	clientexported "github.com/cosmos/cosmos-sdk/x/ibc/02-client/exported"
	connectiontypes "github.com/cosmos/cosmos-sdk/x/ibc/03-connection/types"
	channel "github.com/cosmos/cosmos-sdk/x/ibc/04-channel"
	channelexported "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/exported"
	channeltypes "github.com/cosmos/cosmos-sdk/x/ibc/04-channel/types"
	ibctmtypes "github.com/cosmos/cosmos-sdk/x/ibc/07-tendermint/types"
	"github.com/cosmos/cosmos-sdk/x/staking"

	commitmenttypes "github.com/cosmos/cosmos-sdk/x/ibc/23-commitment/types"
	"github.com/cosmos/cosmos-sdk/x/ibc/ante"
	ibctypes "github.com/cosmos/cosmos-sdk/x/ibc/types"
)

// define constants used for testing
const (
	testChainID    = "test-chain-id"
	testClientIDA  = "testclientida"
	testClientIDB  = "testclientidb"
	testClientType = clientexported.Tendermint

	testConnection = "testconnection"

	testChannelVersion = "1.0"

	height = 10

	trustingPeriod time.Duration = time.Hour * 24 * 7 * 2
	ubdPeriod      time.Duration = time.Hour * 24 * 7 * 3
)

// define variables used for testing
type HandlerTestSuite struct {
	suite.Suite

	cdc *codec.Codec

	chainA *TestChain
	chainB *TestChain
}

func (suite *HandlerTestSuite) SetupTest() {
	suite.chainA = NewTestChain(testClientIDA)
	suite.chainB = NewTestChain(testClientIDB)

	suite.cdc = suite.chainA.App.Codec()

	// create client and connection during setups
	suite.chainA.CreateClient(suite.chainB)
	suite.chainB.CreateClient(suite.chainA)
	suite.chainA.createConnection(testConnection, testConnection, testClientIDB, testClientIDA, ibctypes.OPEN)
	suite.chainB.createConnection(testConnection, testConnection, testClientIDA, testClientIDB, ibctypes.OPEN)
}

func queryProof(chain *TestChain, key string) (proof commitmenttypes.MerkleProof, height int64) {
	res := chain.App.Query(abci.RequestQuery{
		Path:  fmt.Sprintf("store/%s/key", ibctypes.StoreKey),
		Data:  []byte(key),
		Prove: true,
	})

	height = res.Height
	proof = commitmenttypes.MerkleProof{
		Proof: res.Proof,
	}

	return
}

func (suite *HandlerTestSuite) newTx(msg sdk.Msg) sdk.Tx {
	return authtypes.StdTx{
		Msgs: []sdk.Msg{msg},
	}
}

func (suite *HandlerTestSuite) TestHandleMsgPacketOrdered() {
	handler := sdk.ChainAnteDecorators(ante.NewProofVerificationDecorator(
		suite.chainA.App.IBCKeeper.ClientKeeper,
		suite.chainA.App.IBCKeeper.ChannelKeeper,
	))

	packet := channel.NewPacket(newPacket(12345), 1, portid, chanid, cpportid, cpchanid)

	ctx := suite.chainA.GetContext()
	cctx, _ := ctx.CacheContext()
	// suite.chainA.App.IBCKeeper.ChannelKeeper.SetNextSequenceSend(ctx, packet.SourcePort, packet.SourceChannel, 1)
	suite.chainB.App.IBCKeeper.ChannelKeeper.SetPacketCommitment(suite.chainB.GetContext(), packet.SourcePort, packet.SourceChannel, packet.Sequence, channeltypes.CommitPacket(packet.Data.GetPacketDataI()))
	msg := channel.NewMsgPacket(packet, commitmenttypes.MerkleProof{}, 0, addr1)
	_, err := handler(cctx, suite.newTx(msg), false)
	suite.Error(err, "%+v", err) // channel does not exist

	suite.chainA.createChannel(cpportid, cpchanid, portid, chanid, ibctypes.OPEN, ibctypes.ORDERED, testConnection)
	suite.chainB.createChannel(portid, chanid, cpportid, cpchanid, ibctypes.OPEN, ibctypes.ORDERED, testConnection)
	ctx = suite.chainA.GetContext()
	packetCommitmentPath := ibctypes.PacketCommitmentPath(packet.SourcePort, packet.SourceChannel, packet.Sequence)
	proof, proofHeight := queryProof(suite.chainB, packetCommitmentPath)
	msg = channel.NewMsgPacket(packet, proof, uint64(proofHeight), addr1)
	_, err = handler(cctx, suite.newTx(msg), false)
	suite.Error(err, "%+v", err) // invalid proof

	suite.chainA.updateClient(suite.chainB)
	// // commit chainA to flush to IAVL so we can get proof
	// suite.chainA.App.Commit()
	// suite.chainA.App.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: suite.chainA.App.LastBlockHeight() + 1, Time: suite.chainA.Header.GetTime()}})
	// ctx = suite.chainA.GetContext()

	proof, proofHeight = queryProof(suite.chainB, packetCommitmentPath)
	msg = channel.NewMsgPacket(packet, proof, uint64(proofHeight), addr1)

	for i := 0; i < 10; i++ {
		cctx, write := suite.chainA.GetContext().CacheContext()
		suite.chainA.App.IBCKeeper.ChannelKeeper.SetNextSequenceRecv(cctx, cpportid, cpchanid, uint64(i))
		_, err := handler(cctx, suite.newTx(msg), false)
		if err == nil {
			err = suite.chainA.App.IBCKeeper.ChannelKeeper.PacketExecuted(cctx, packet, packet.Data.GetPacketDataI())
		}
		if i == 1 {
			suite.NoError(err, "%d", i) // successfully executed
			write()
		} else {
			suite.Error(err, "%d", i) // wrong incoming sequence
		}
	}
}

func (suite *HandlerTestSuite) TestHandleMsgPacketUnordered() {
	handler := sdk.ChainAnteDecorators(ante.NewProofVerificationDecorator(
		suite.chainA.App.IBCKeeper.ClientKeeper,
		suite.chainA.App.IBCKeeper.ChannelKeeper,
	))

	// Not testing nonexist channel, invalid proof, nextseqsend, they are already tested in TestHandleMsgPacketOrdered

	var packet channeltypes.Packet
	for i := 0; i < 5; i++ {
		packet = channel.NewPacket(newPacket(uint64(i)), uint64(i), portid, chanid, cpportid, cpchanid)
		suite.chainB.App.IBCKeeper.ChannelKeeper.SetPacketCommitment(suite.chainB.GetContext(), packet.SourcePort, packet.SourceChannel, uint64(i), channeltypes.CommitPacket(packet.Data.GetPacketDataI()))
	}

	// suite.chainA.App.IBCKeeper.ChannelKeeper.SetNextSequenceSend(suite.chainA.GetContext(), packet.SourcePort, packet.SourceChannel, uint64(10))

	suite.chainA.createChannel(cpportid, cpchanid, portid, chanid, ibctypes.OPEN, ibctypes.UNORDERED, testConnection)

	suite.chainA.updateClient(suite.chainB)

	for i := 10; i >= 0; i-- {
		cctx, write := suite.chainA.GetContext().CacheContext()
		packet = channel.NewPacket(newPacket(uint64(i)), uint64(i), portid, chanid, cpportid, cpchanid)
		packetCommitmentPath := ibctypes.PacketCommitmentPath(packet.SourcePort, packet.SourceChannel, uint64(i))
		proof, proofHeight := queryProof(suite.chainB, packetCommitmentPath)
		msg := channel.NewMsgPacket(packet, proof, uint64(proofHeight), addr1)
		_, err := handler(cctx, suite.newTx(msg), false)
		if i < 5 {
			suite.NoError(err, "%d", i) // successfully executed
			write()
		} else {
			suite.Error(err, "%d", i) // wrong incoming sequence
		}
	}
}

func TestHandlerTestSuite(t *testing.T) {
	suite.Run(t, new(HandlerTestSuite))
}

type TestChain struct {
	ClientID string
	App      *simapp.SimApp
	Header   ibctmtypes.Header
	Vals     *tmtypes.ValidatorSet
	Signers  []tmtypes.PrivValidator
}

func NewTestChain(clientID string) *TestChain {
	privVal := tmtypes.NewMockPV()
	validator := tmtypes.NewValidator(privVal.GetPubKey(), 1)
	valSet := tmtypes.NewValidatorSet([]*tmtypes.Validator{validator})
	signers := []tmtypes.PrivValidator{privVal}
	now := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)

	header := ibctmtypes.CreateTestHeader(clientID, 1, now, valSet, signers)

	return &TestChain{
		ClientID: clientID,
		App:      simapp.Setup(false),
		Header:   header,
		Vals:     valSet,
		Signers:  signers,
	}
}

// Creates simple context for testing purposes
func (chain *TestChain) GetContext() sdk.Context {
	return chain.App.BaseApp.NewContext(false, abci.Header{ChainID: chain.Header.SignedHeader.Header.ChainID, Height: int64(chain.Header.GetHeight())})
}

// createClient will create a client for clientChain on targetChain
func (chain *TestChain) CreateClient(client *TestChain) error {
	client.Header = nextHeader(client)
	// Commit and create a new block on appTarget to get a fresh CommitID
	client.App.Commit()
	commitID := client.App.LastCommitID()
	client.App.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: int64(client.Header.GetHeight()), Time: client.Header.GetTime()}})

	// Set HistoricalInfo on client chain after Commit
	ctxClient := client.GetContext()
	validator := staking.NewValidator(
		sdk.ValAddress(client.Vals.Validators[0].Address), client.Vals.Validators[0].PubKey, staking.Description{},
	)
	validator.Status = sdk.Bonded
	validator.Tokens = sdk.NewInt(1000000) // get one voting power
	validators := []staking.Validator{validator}
	histInfo := staking.HistoricalInfo{
		Header: abci.Header{
			AppHash: commitID.Hash,
		},
		Valset: validators,
	}
	client.App.StakingKeeper.SetHistoricalInfo(ctxClient, int64(client.Header.GetHeight()), histInfo)

	// Create target ctx
	ctxTarget := chain.GetContext()

	// create client
	clientState, err := ibctmtypes.Initialize(client.ClientID, trustingPeriod, ubdPeriod, client.Header)
	if err != nil {
		return err
	}
	_, err = chain.App.IBCKeeper.ClientKeeper.CreateClient(ctxTarget, clientState, client.Header.ConsensusState())
	if err != nil {
		return err
	}
	return nil

	// _, _, err := simapp.SignCheckDeliver(
	// 	suite.T(),
	// 	suite.cdc,
	// 	suite.chainA.App.BaseApp,
	// 	ctx.BlockHeader(),
	// 	[]sdk.Msg{clienttypes.NewMsgCreateClient(clientID, clientexported.ClientTypeTendermint, consState, accountAddress)},
	// 	[]uint64{baseAccount.GetAccountNumber()},
	// 	[]uint64{baseAccount.GetSequence()},
	// 	true, true, accountPrivKey,
	// )
}

func (chain *TestChain) updateClient(client *TestChain) {
	// Create target ctx
	ctxTarget := chain.GetContext()

	// if clientState does not already exist, return without updating
	_, found := chain.App.IBCKeeper.ClientKeeper.GetClientState(
		ctxTarget, client.ClientID,
	)
	if !found {
		return
	}

	// always commit when updateClient and begin a new block
	client.App.Commit()
	commitID := client.App.LastCommitID()

	client.Header = nextHeader(client)
	client.App.BeginBlock(abci.RequestBeginBlock{Header: abci.Header{Height: int64(client.Header.GetHeight()), Time: client.Header.GetTime()}})

	// Set HistoricalInfo on client chain after Commit
	ctxClient := client.GetContext()
	validator := staking.NewValidator(
		sdk.ValAddress(client.Vals.Validators[0].Address), client.Vals.Validators[0].PubKey, staking.Description{},
	)
	validator.Status = sdk.Bonded
	validator.Tokens = sdk.NewInt(1000000)
	validators := []staking.Validator{validator}
	histInfo := staking.HistoricalInfo{
		Header: abci.Header{
			AppHash: commitID.Hash,
		},
		Valset: validators,
	}
	client.App.StakingKeeper.SetHistoricalInfo(ctxClient, int64(client.Header.GetHeight()), histInfo)

	consensusState := ibctmtypes.ConsensusState{
		Height:       client.Header.GetHeight() - 1,
		Timestamp:    client.Header.GetTime(),
		Root:         commitmenttypes.NewMerkleRoot(commitID.Hash),
		ValidatorSet: client.Vals,
	}

	chain.App.IBCKeeper.ClientKeeper.SetClientConsensusState(
		ctxTarget, client.ClientID, client.Header.GetHeight()-1, consensusState,
	)
	chain.App.IBCKeeper.ClientKeeper.SetClientState(
		ctxTarget, ibctmtypes.NewClientState(client.ClientID, trustingPeriod, ubdPeriod, client.Header),
	)

	// _, _, err := simapp.SignCheckDeliver(
	// 	suite.T(),
	// 	suite.cdc,
	// 	suite.chainA.App.BaseApp,
	// 	ctx.BlockHeader(),
	// 	[]sdk.Msg{clienttypes.NewMsgUpdateClient(clientID, suite.header, accountAddress)},
	// 	[]uint64{baseAccount.GetAccountNumber()},
	// 	[]uint64{baseAccount.GetSequence()},
	// 	true, true, accountPrivKey,
	// )
	// suite.Require().NoError(err)
}

func (chain *TestChain) createConnection(
	connID, counterpartyConnID, clientID, counterpartyClientID string,
	state ibctypes.State,
) connectiontypes.ConnectionEnd {
	counterparty := connectiontypes.NewCounterparty(counterpartyClientID, counterpartyConnID, chain.App.IBCKeeper.ConnectionKeeper.GetCommitmentPrefix())
	connection := connectiontypes.ConnectionEnd{
		State:        state,
		ClientID:     clientID,
		Counterparty: counterparty,
		Versions:     connectiontypes.GetCompatibleVersions(),
	}
	ctx := chain.GetContext()
	chain.App.IBCKeeper.ConnectionKeeper.SetConnection(ctx, connID, connection)
	return connection
}

func (chain *TestChain) createChannel(
	portID, channelID, counterpartyPortID, counterpartyChannelID string,
	state ibctypes.State, order ibctypes.Order, connectionID string,
) channeltypes.Channel {
	counterparty := channeltypes.NewCounterparty(counterpartyPortID, counterpartyChannelID)
	channel := channeltypes.NewChannel(state, order, counterparty,
		[]string{connectionID}, "1.0",
	)
	ctx := chain.GetContext()
	chain.App.IBCKeeper.ChannelKeeper.SetChannel(ctx, portID, channelID, channel)
	return channel
}

func nextHeader(chain *TestChain) ibctmtypes.Header {
	return ibctmtypes.CreateTestHeader(
		chain.Header.SignedHeader.Header.ChainID, int64(chain.Header.GetHeight())+1,
		chain.Header.GetTime().Add(time.Minute), chain.Vals, chain.Signers,
	)
}

var _ channelexported.PacketDataI = packetT{}

type packetT struct {
	Data uint64
}

func (packet packetT) GetBytes() []byte {
	return []byte(fmt.Sprintf("%d", packet.Data))
}

func (packetT) GetTimeoutHeight() uint64 {
	return 100
}

func (packetT) ValidateBasic() error {
	return nil
}

func (packetT) Type() string {
	return "valid"
}

func newPacket(data uint64) packetT {
	return packetT{data}
}

// define variables used for testing
var (
	addr1 = sdk.AccAddress("testaddr1")

	portid   = "testportid"
	chanid   = "testchannel"
	cpportid = "testcpport"
	cpchanid = "testcpchannel"
)
