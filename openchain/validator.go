/*
Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements.  See the NOTICE file
distributed with this work for additional information
regarding copyright ownership.  The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License.  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied.  See the License for the
specific language governing permissions and limitations
under the License.
*/

package openchain

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"golang.org/x/net/context"
	"google.golang.org/grpc/grpclog"

	"github.com/golang/protobuf/proto"
	"github.com/looplab/fsm"
	"github.com/op/go-logging"
	"github.com/spf13/viper"

	"github.com/openblockchain/obc-peer/openchain/consensus/pbft"
	"github.com/openblockchain/obc-peer/openchain/ledger"
	"github.com/openblockchain/obc-peer/openchain/util"
	pb "github.com/openblockchain/obc-peer/protos"
)

var validatorLogger = logging.MustGetLogger("validator")

type Validator interface {
	Broadcast(*pb.OpenchainMessage) error
	GetHandler(stream PeerChatStream) MessageHandler
	IsLeader() bool
}

type SimpleValidator struct {
	validatorStreams map[string]MessageHandler
	peerStreams      map[string]MessageHandler
	leaderHandler    MessageHandler // handler representing either side of stream
	isLeader         bool
}

func (v *SimpleValidator) IsLeader() bool {
	return v.isLeader
}

func (v *SimpleValidator) Broadcast(msg *pb.OpenchainMessage) error {
	validatorLogger.Debug("Broadcasting OpenchainMessage of type: %s", msg.Type)
	err := v.leaderHandler.SendMessage(msg)
	if err != nil {
		return fmt.Errorf("Error broadcasting msg of type: %s", msg.Type)
	}
	return nil
}

func (v *SimpleValidator) GetHandler(stream PeerChatStream) MessageHandler {
	if v.isLeader {
		v.leaderHandler = NewValidatorFSM(v, "", stream)
		return v.leaderHandler
	}
	return NewValidatorFSM(v, "", stream)
}

func (v *SimpleValidator) chatWithLeader(peerAddress string) error {

	var errFromChat error = nil
	conn, err := NewPeerClientConnectionWithAddress(peerAddress)
	if err != nil {
		return errors.New(fmt.Sprintf("Error connecting to leader at address=%s:  %s", peerAddress, err))
	}
	serverClient := pb.NewPeerClient(conn)
	stream, err := serverClient.Chat(context.Background())
	v.leaderHandler = v.GetHandler(stream)

	if err != nil {
		return errors.New(fmt.Sprintf("Error chatting with leader at address=%s:  %s", peerAddress, err))
	} else {
		defer stream.CloseSend()
		stream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_HELLO})
		waitc := make(chan struct{})
		go func() {
			for {
				in, err := stream.Recv()
				if err == io.EOF {
					// read done.
					errFromChat = errors.New(fmt.Sprintf("Error sending transactions to peer address=%s, received EOF when expecting %s", peerAddress, pb.OpenchainMessage_DISC_HELLO))
					close(waitc)
					return
				}
				if err != nil {
					grpclog.Fatalf("Failed to receive a DiscoverMessage from server : %v", err)
				}
				// Call FSM.HandleMessage()
				err = v.leaderHandler.HandleMessage(in)
				if err != nil {
					validatorLogger.Error("Error handling message: %s", err)
					return
				}

				// 	if in.Type == pb.OpenchainMessage_DISC_HELLO {
				// 		peerLogger.Debug("Received %s message as expected, sending transactions...", in.Type)
				// 		payload, err := proto.Marshal(transactionsMessage)
				// 		if err != nil {
				// 			errFromChat = errors.New(fmt.Sprintf("Error marshalling transactions to peer address=%s:  %s", peerAddress, err))
				// 			close(waitc)
				// 			return
				// 		}
				// 		stream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_CHAIN_TRANSACTIONS, Payload: payload})
				// 		peerLogger.Debug("Transactions sent to peer address: %s", peerAddress)
				// 		close(waitc)
				// 		return
				// 	} else {
				// 		peerLogger.Debug("Got unexpected message %s, with bytes length = %d,  doing nothing", in.Type, len(in.Payload))
				// 		close(waitc)
				// 		return
				// 	}
			}
		}()
		<-waitc
		return nil
	}
}

func NewSimpleValidator(isLeader bool) (Validator, error) {
	validator := &SimpleValidator{}
	// Only perform if NOT the leader
	if !viper.GetBool("peer.consensus.leader.enabled") {
		leaderAddress := viper.GetString("peer.consensus.leader.address")
		validatorLogger.Debug("Creating client to Peer (Leader) with address: %s", leaderAddress)
		go validator.chatWithLeader(leaderAddress)
	}
	validator.isLeader = isLeader
	return validator, nil
}

type ValidatorFSM struct {
	To             string
	ChatStream     PeerChatStream
	FSM            *fsm.FSM
	PeerFSM        *PeerFSM
	validator      Validator
	storedRequests map[string]*pbft.PBFT
}

func NewValidatorFSM(parent Validator, to string, peerChatStream PeerChatStream) *ValidatorFSM {
	v := &ValidatorFSM{
		To:         to,
		ChatStream: peerChatStream,
		validator:  parent,
	}
	v.storedRequests = make(map[string]*pbft.PBFT)

	v.FSM = fsm.NewFSM(
		"created",
		fsm.Events{
			{Name: pb.OpenchainMessage_DISC_HELLO.String(), Src: []string{"created"}, Dst: "established"},
			{Name: pb.OpenchainMessage_CHAIN_TRANSACTIONS.String(), Src: []string{"established"}, Dst: "established"},
			{Name: pbft.PBFT_REQUEST.String(), Src: []string{"established"}, Dst: "prepare_result_sent"},
			{Name: pbft.PBFT_PRE_PREPARE.String(), Src: []string{"established"}, Dst: "prepare_result_sent"},
			{Name: pbft.PBFT_PREPARE_RESULT.String(), Src: []string{"prepare_result_sent"}, Dst: "commit_result_sent"},
			{Name: pbft.PBFT_COMMIT_RESULT.String(), Src: []string{"prepare_result_sent", "commit_result_sent"}, Dst: "committed_block"},
		},
		fsm.Callbacks{
			"before_" + pb.OpenchainMessage_DISC_HELLO.String():         func(e *fsm.Event) { v.beforeHello(e) },
			"before_" + pb.OpenchainMessage_CHAIN_TRANSACTIONS.String(): func(e *fsm.Event) { v.beforeChainTransactions(e) },
			"before_" + pbft.PBFT_REQUEST.String():                      func(e *fsm.Event) { v.beforeRequest(e) },
			"before_" + pbft.PBFT_PRE_PREPARE.String():                  func(e *fsm.Event) { v.beforePrePrepareResult(e) },
			"before_" + pbft.PBFT_PREPARE_RESULT.String():               func(e *fsm.Event) { v.beforePrepareResult(e) },
			"before_" + pbft.PBFT_COMMIT_RESULT.String():                func(e *fsm.Event) { v.beforeCommitResult(e) },
		},
	)
	return v
}

func (v *ValidatorFSM) enterState(e *fsm.Event) {
	validatorLogger.Debug("The Validators's bi-directional stream to %s is %s, from event %s\n", v.To, e.Dst, e.Event)
}

func (v *ValidatorFSM) beforeHello(e *fsm.Event) {
	validatorLogger.Debug("Sending back %s", pb.OpenchainMessage_DISC_HELLO.String())
	if err := v.ChatStream.Send(&pb.OpenchainMessage{Type: pb.OpenchainMessage_DISC_HELLO}); err != nil {
		e.Cancel(err)
	}
}

func (v *ValidatorFSM) beforeChainTransactions(e *fsm.Event) {
	validatorLogger.Debug("Sending broadcast to all validators upon receipt of %s", e.Event)
	if _, ok := e.Args[0].(*pb.OpenchainMessage); !ok {
		e.Cancel(fmt.Errorf("Received unexpected message type"))
		return
	}
	msg := e.Args[0].(*pb.OpenchainMessage)

	uuid, err := util.GenerateUUID()
	if err != nil {
		e.Cancel(fmt.Errorf("Error generating UUID: %s", err))
		return
	}
	// For now unpack the lone transaction and send as the payload
	transactionsMessage := &pb.TransactionsMessage{}
	err = proto.Unmarshal(msg.Payload, transactionsMessage)
	if err != nil {
		e.Cancel(fmt.Errorf("Error generating UUID: %s", err))
		return
	}
	// Currently expect only 1 transaction in TransactionsMessage
	validatorLogger.Warning("Currently expect exactly 1 transaction in TransactionsMessage")
	numOfTransactions := len(transactionsMessage.Transactions)
	if numOfTransactions != 1 {
		e.Cancel(fmt.Errorf("Expected exactly one transaction in TransactionsMessage.Transactions, received %d", numOfTransactions))
		return
	}
	transactionToSend := transactionsMessage.Transactions[0]
	data, err := proto.Marshal(transactionToSend)
	if err != nil {
		e.Cancel(fmt.Errorf("Error marshalling transaction to PBFT struct: %s", err))
		return
	}
	pbftData, err := proto.Marshal(&pbft.PBFT{Type: pbft.PBFT_REQUEST, ID: uuid, Payload: data})
	if err != nil {
		e.Cancel(fmt.Errorf("Error marshalling pbft: %s", err))
		return
	}
	newMsg := &pb.OpenchainMessage{Type: pb.OpenchainMessage_CONSENSUS, Payload: pbftData}
	validatorLogger.Debug("Broadcasting %s with PBFT type: %s", pb.OpenchainMessage_CONSENSUS, pbft.PBFT_REQUEST)
	v.validator.Broadcast(newMsg)
}

func (v *ValidatorFSM) when(stateToCheck string) bool {
	return v.FSM.Is(stateToCheck)
}

func (v *ValidatorFSM) HandleMessage(msg *pb.OpenchainMessage) error {
	validatorLogger.Debug("Handling OpenchainMessage of type: %s ", msg.Type)
	if viper.GetBool("peer.consensus.leader.enabled") {
		validatorLogger.Debug("Leader's storedRequests length = %d", len(v.storedRequests))
	}
	if msg.Type == pb.OpenchainMessage_CONSENSUS {
		pbft := &pbft.PBFT{}
		err := proto.Unmarshal(msg.Payload, pbft)
		if err != nil {
			return fmt.Errorf("Error unpacking Payload from %s message: %s", pb.OpenchainMessage_CONSENSUS, err)
		}
		validatorLogger.Debug("Handling msg %s with PBFT type: %s", msg.Type, pbft.Type)
		if v.FSM.Cannot(pbft.Type.String()) {
			return fmt.Errorf("Validator FSM cannot handle %s message (%s) with payload size (%d) while in state: %s", pb.OpenchainMessage_CONSENSUS, pbft.Type.String(), len(pbft.Payload), v.FSM.Current())
		}
		err = v.FSM.Event(pbft.Type.String(), pbft)
		validatorLogger.Debug("Processed msg %s with PBFT type: %s, current state: %s", msg.Type, pbft.Type, v.FSM.Current())
		return filterError(err)
	} else {
		if v.FSM.Cannot(msg.Type.String()) {
			return fmt.Errorf("Validator FSM cannot handle message (%s) with payload size (%d) while in state: %s", msg.Type.String(), len(msg.Payload), v.FSM.Current())
		}
		err := v.FSM.Event(msg.Type.String(), msg)
		return filterError(err)
	}
}

// Filter the Errors to allow NoTransitionError and CanceledError to not propogate for cases where embedded Err == nil
func filterError(errFromFSMEvent error) error {
	if errFromFSMEvent != nil {
		if noTransitionErr, ok := errFromFSMEvent.(*fsm.NoTransitionError); ok {
			if noTransitionErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return errFromFSMEvent
				//t.Error("expected only 'NoTransitionError'")
			} else {
				validatorLogger.Debug("Ignoring NoTransitionError: %s", noTransitionErr)
			}
		}
		if canceledErr, ok := errFromFSMEvent.(*fsm.CanceledError); ok {
			if canceledErr.Err != nil {
				// Only allow NoTransitionError's, all others are considered true error.
				return canceledErr
				//t.Error("expected only 'NoTransitionError'")
			} else {
				validatorLogger.Debug("Ignoring CanceledError: %s", canceledErr)
			}
		}
	}
	return nil
}

func (v *ValidatorFSM) SendMessage(msg *pb.OpenchainMessage) error {
	validatorLogger.Debug("Sending message to stream of type: %s ", msg.Type)
	err := v.ChatStream.Send(msg)
	if err != nil {
		return fmt.Errorf("Error Sending message through ChatStream: %s", err)
	}
	return nil
}

func (v *ValidatorFSM) beforeRequest(e *fsm.Event) {
	validatorLogger.Debug("Handling beforeRequest for event: %s", e.Event)
	// Check incoming message.
	if _, ok := e.Args[0].(*pbft.PBFT); !ok {
		e.Cancel(fmt.Errorf("Unexpected message received."))
		return
	}
	message := e.Args[0].(*pbft.PBFT)
	// Calculate hash.
	hash := util.ComputeCryptoHash(message.Payload)
	hashString := base64.StdEncoding.EncodeToString(hash)
	// Store in map.
	validatorLogger.Debug("Before storing requests in map for event: %s", e.Event)
	if _, ok := v.storedRequests[hashString]; ok {
		e.Cancel(fmt.Errorf("Message (hash: %v) already stored,", hashString))
		return
	} else {
		v.storedRequests[hashString] = message
		validatorLogger.Debug("Stored newRequest in map (length %d) under key (%s), value isNil (%v)", len(v.storedRequests), hashString, message == nil)
	}
	if v.validator.IsLeader() {
		v.broadcastPrePrepareAndPrepare(e)
	} else {
		// Cancel transition if NOT leader
		validatorLogger.Debug("Cancelling transition of non-leader for event: %s", e.Event)
		e.Cancel()
	}

}

func (v *ValidatorFSM) beforePrePrepareResult(e *fsm.Event) {
	// Check incoming message.
	if _, ok := e.Args[0].(*pbft.PBFT); !ok {
		e.Cancel(fmt.Errorf("Unexpected message received."))
		return
	}
	msg := e.Args[0].(*pbft.PBFT)

	// Don't do anything if LEADER
	if v.validator.IsLeader() {
		validatorLogger.Debug("Cancelling transition for leader due to event: %s", e.Event)
		e.Cancel()
		return
	}

	// Get transactions from message
	pbftArray := &pbft.PBFTArray{}
	err := proto.Unmarshal(msg.Payload, pbftArray)
	if err != nil {
		e.Cancel(fmt.Errorf("Error unmarshaling PBFTArray: %s", err))
		return
	}
	transactions, err := convertPBFTsToTransactions(pbftArray)
	if err != nil {
		e.Cancel(fmt.Errorf("Error converting PBFTs to Transactions: %s", err))
		return
	}

	// Execute transactions
	hopefulHash, errs := executeTransactions(context.Background(), transactions)
	for _, currErr := range errs {
		if currErr != nil {
			e.Cancel(fmt.Errorf("Error executing transactions pbft: %s", currErr))
		}
	}
	//continue even if errors if hash is not nil
	if hopefulHash == nil {
		e.Cancel(fmt.Errorf("nil hash not broadcasting hash result"))
		return
	}
	//Don't care about ID string for now
	var id string
	prepres, err := proto.Marshal(&pbft.PBFT{Type: pbft.PBFT_PREPARE_RESULT, ID: id, Payload: hopefulHash})
	if err != nil {
		e.Cancel(fmt.Errorf("Error marshalling pbft: %s", err))
		return
	}
	newMsg := &pb.OpenchainMessage{Type: pb.OpenchainMessage_CONSENSUS, Payload: prepres}
	validatorLogger.Debug("Broadcasting %s after receiving message type : %s", pbft.PBFT_PREPARE_RESULT, e.Event)
	v.validator.Broadcast(newMsg)
	// TODO: Various checks should go here -- skipped for now.
	// TODO: Execute transactions in PRE_PREPARE using Murali's code.
	// TODO: Create OpenchainMessage_CONSENSUS message where PAYLOAD is a PHASE:PREPARE_RESULT message.
}

func (v *ValidatorFSM) beforePrepareResult(e *fsm.Event) {
	// Check incoming message.
	if _, ok := e.Args[0].(*pbft.PBFT); !ok {
		e.Cancel(fmt.Errorf("Unexpected message received."))
		return
	}
	msg := e.Args[0].(*pbft.PBFT)
	validatorLogger.Debug("TODO: Create OpenchainMessage_CONSENSUS message where PAYLOAD is a PHASE:COMMIT_RESULT message after msg of type: %s", msg.Type)
	// TODO: Various checks should go here -- skipped for now.
	// TODO: Create OpenchainMessage_CONSENSUS message where PAYLOAD is a PHASE:COMMIT_RESULT message.
	// Simply clone received PBFT message and change type, may need to be revisited
	commitResult, err := proto.Marshal(&pbft.PBFT{Type: pbft.PBFT_COMMIT_RESULT, ID: msg.ID, Payload: msg.Payload})
	if err != nil {
		e.Cancel(fmt.Errorf("Error marshalling pbft: %s", err))
		return
	}
	newMsg := &pb.OpenchainMessage{Type: pb.OpenchainMessage_CONSENSUS, Payload: commitResult}
	validatorLogger.Debug("Getting ready to broadcast %s after receiving message type : %s", pbft.PBFT_COMMIT_RESULT, msg.Type)
	v.validator.Broadcast(newMsg)
}

func (v *ValidatorFSM) beforeCommitResult(e *fsm.Event) {
	// Check incoming message.
	if _, ok := e.Args[0].(*pbft.PBFT); !ok {
		e.Cancel(fmt.Errorf("Unexpected message received."))
		return
	}
	msg := e.Args[0].(*pbft.PBFT)
	if e.FSM.Current() != "commit_result_sent" {
		// Only send if have NOT already sent
		validatorLogger.Debug("TODO: Validator received %s, needs to send its own %s", msg.Type, pbft.PBFT_COMMIT_RESULT)
		//v.validator.Broadcast(CommitResult)
	}
	// TODO: Now commitToBlockchain()
	validatorLogger.Debug("TODO: Now commitToBlockchain(), received %s while in state: %s", e.Event, e.FSM.Current())
	// TODO: Various checks should go here -- skipped for now.
	// TODO: Commit referenced transactions to blockchain, uncomment Broadcast above.
}

func (v *ValidatorFSM) broadcastPrePrepareAndPrepare(e *fsm.Event) {
	// How many Requests currently in map?
	storedCount := len(v.storedRequests)
	if storedCount >= 2 {
		// First: Broadcast OpenchainMessage_CONSENSUS with PAYLOAD: PRE_PREPARE.
		pbfts := make([]*pbft.PBFT, 0)
		for _, pbft := range v.storedRequests {
			pbfts = append(pbfts, pbft)
		}
		// Marshal the array of Request messages.
		data1, err := proto.Marshal(&pbft.PBFTArray{Pbfts: pbfts})
		if err != nil {
			e.Cancel(fmt.Errorf("Error marshalling array of Request messages: %s", err))
			return
		}
		// Marshal the PRE_PREPARE message.
		data2, err := proto.Marshal(&pbft.PBFT{Type: pbft.PBFT_PRE_PREPARE, ID: "nil", Payload: data1})
		if err != nil {
			e.Cancel(fmt.Errorf("Error marshalling PRE_PREPARE message: %s", err))
			return
		}
		// Create new consensus message.
		newMsg := &pb.OpenchainMessage{Type: pb.OpenchainMessage_CONSENSUS, Payload: data2}
		validatorLogger.Debug("Broadcasting %s after receiving msg: %s", pbft.PBFT_PRE_PREPARE, e.Event)
		v.validator.Broadcast(newMsg)

		// TODO: Various checks should go here -- skipped for now.
		// TODO: Execute transactions in PRE_PREPARE using Murali's code.
		// TODO: Create OpenchainMessage_CONSENSUS message where PAYLOAD is a PHASE:PREPARE_RESULT message.
		//validatorLogger.Debug("TODO: Execute transactions in PRE_PREPARE using Murali's code, then Broadcast %s", pbft.PBFT_PREPARE_RESULT)
		pbftArray := &pbft.PBFTArray{Pbfts: pbfts}
		transactions, err := convertPBFTsToTransactions(pbftArray)
		if err != nil {
			e.Cancel(fmt.Errorf("Error converting PBFTs to Transactions: %s", err))
			return
		}

		// Execute transactions
		hopefulHash, errs := executeTransactions(context.Background(), transactions)
		for _, currErr := range errs {
			if currErr != nil {
				e.Cancel(fmt.Errorf("Error executing transactions pbft: %s", currErr))
			}
		}
		//continue even if errors if hash is not nil
		if hopefulHash == nil {
			e.Cancel(fmt.Errorf("nil hash not broadcasting hash result"))
			return
		}
		validatorLogger.Debug("TODO: Executed transactions, now need to Broadcast %s", pbft.PBFT_PREPARE_RESULT)

	} else {
		validatorLogger.Debug("StoredCount = %d, Leader going to remain in %s.", storedCount, e.FSM.Current())
		e.Cancel()
	}
}

func convertPBFTsToTransactions(pbftArray *pbft.PBFTArray) ([]*pb.Transaction, error) {
	transactions := make([]*pb.Transaction, 0)
	for _, pbft := range pbftArray.Pbfts {
		tx := &pb.Transaction{}
		err := proto.Unmarshal(pbft.Payload, tx)
		if err != nil {
			return nil, fmt.Errorf("Error unmarshalling transaction from PBFT: %s", err)
		}
		transactions = append(transactions, tx)
	}
	return transactions, nil
}

//executeTransactions - will execute transactions on the array one by one
//will return an array of errors one for each transaction. If the execution
//succeeded, array element will be nil. returns state hash
func executeTransactions(ctxt context.Context, xacts []*pb.Transaction) ([]byte, []error) {
	//1 for GetState().GetHash()
	errs := make([]error, len(xacts)+1)
	for i, t := range xacts {
		//add "function" as an argument to be passed
		newArgs := make([]string, len(t.Args)+1)
		newArgs[0] = t.Function
		copy(newArgs[1:len(t.Args)+1], t.Args)
		//is there a payload to be passed to the container ?
		var buf *bytes.Buffer
		if t.Payload != nil {
			buf = bytes.NewBuffer(t.Payload)
		}
		cds := &pb.ChainletDeploymentSpec{}
		errs[i] = proto.Unmarshal(t.Payload, cds)
		if errs[i] != nil {
			continue
		}
		//create start request ...
		var req VMCReqIntf
		vmname, berr := buildVMName(cds.ChainletSpec)
		if berr != nil {
			errs[i] = berr
			continue
		}
		if t.Type == pb.Transaction_CHAINLET_NEW {
			var targz io.Reader = bytes.NewBuffer(cds.CodePackage)
			req = CreateImageReq{Id: vmname, Args: newArgs, Reader: targz}
		} else if t.Type == pb.Transaction_CHAINLET_EXECUTE {
			req = StartImageReq{Id: vmname, Args: newArgs, Instream: buf}
		} else {
			errs[i] = fmt.Errorf("Invalid transaction type %s", t.Type.String())
		}
		//... and execute it. err will be nil if successful
		_, errs[i] = VMCProcess(ctxt, DOCKER, req)
	}
	//TODO - error processing ... for now assume everything worked
	ledger, hasherr := ledger.GetLedger()
	var statehash []byte
	if hasherr == nil {
		statehash, hasherr = ledger.GetTempStateHash()
	}
	errs[len(errs)-1] = hasherr
	return statehash, errs
}
