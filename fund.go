package main

import (
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/uspv"
	"github.com/lightningnetwork/lnd/uspv/uwire"
)

// FundChannel makes a multisig address with the node connected to...
// first just request one of their pubkeys (1 byte message)
func FundChannel(args []string) error {
	if RemoteCon == nil {
		return fmt.Errorf("Not connected to anyone\n")
	}
	msg := []byte{uwire.MSGID_PUBREQ}
	_, err := RemoteCon.Write(msg)
	return err
}

// PubReqHandler gets a (content-less) pubkey request.  Respond with a pubkey
// note that this only causes a disk read, not a disk write.
// so if someone sends 10 pubkeyreqs, they'll get the same pubkey back 10 times.
// they have to provide an actual tx before the next pubkey will come out.
func PubReqHandler(from [16]byte) {
	// pub req; check that idx matches next idx of ours and create pubkey
	peerBytes := RemoteCon.RemotePub.SerializeCompressed()
	pub, err := SCon.TS.NextPubForPeer(peerBytes)
	if err != nil {
		fmt.Printf("MultiReqHandler err %s", err.Error())
		return
	}
	fmt.Printf("Generated pubkey %x\n", pub)
	msg := []byte{uwire.MSGID_PUBRESP}
	msg = append(msg, pub...)

	_, err = RemoteCon.Write(msg)
	return
}

// PubRespHandler -once the pubkey response comes back, we can create the
// transaction.  Create, save to DB, sign and send over the wire (and broadcast)
func PubRespHandler(from [16]byte, theirPubBytes []byte) {
	qChanCapacity := int64(2000000) // this will be an arg
	satPerByte := int64(80)
	capBytes := uspv.I64tB(qChanCapacity)

	// make sure their pubkey is a pubkey
	theirPub, err := btcec.ParsePubKey(theirPubBytes, btcec.S256())
	if err != nil {
		fmt.Printf("PubRespHandler err %s", err.Error())
		return
	}

	fmt.Printf("got pubkey response %x\n", theirPub.SerializeCompressed())

	tx := wire.NewMsgTx() // make new tx
	//	tx.Flags = 0x01       // tx will be witty

	// first get inputs. comes sorted from PickUtxos.
	utxos, overshoot, err := SCon.PickUtxos(qChanCapacity, true)
	if err != nil {
		fmt.Printf("PubRespHandler err %s", err.Error())
		return
	}
	if overshoot < 0 {
		fmt.Printf("witness utxos undershoot by %d", -overshoot)
		return
	}
	// add all the inputs to the tx
	for _, utxo := range utxos {
		tx.AddTxIn(wire.NewTxIn(&utxo.Op, nil, nil))
	}
	// estimate fee
	fee := uspv.EstFee(tx, satPerByte)
	// create change output
	changeOut, err := SCon.TS.NewChangeOut(overshoot - fee)
	if err != nil {
		fmt.Printf("PubRespHandler err %s", err.Error())
		return
	}

	tx.AddTxOut(changeOut) // add change output

	//	fmt.Printf("overshoot %d pub idx %d; made output script: %x\n",
	//		overshoot, idx, multiOut.PkScript)

	peerBytes := RemoteCon.RemotePub.SerializeCompressed()
	// send partial tx to db to be saved and have output populated
	op, myPubBytes, err := SCon.TS.MakeFundTx(
		tx, qChanCapacity, peerBytes, theirPub)
	if err != nil {
		fmt.Printf("PubRespHandler err %s", err.Error())
		return
	}
	// don't need to add to filters; we'll pick the TX up anyway because it
	// spends our utxos.

	// tx saved in DB.  Next then notify peer (then sign and broadcast)
	fmt.Printf("tx:%s ", uspv.TxToString(tx))

	// description is outpoint (36), myPubkey(33), multisig capacity (8)
	msg := []byte{uwire.MSGID_MULTIDESC}
	msg = append(msg, uspv.OutPointToBytes(*op)...)
	msg = append(msg, myPubBytes...)
	// do you actually need to say the capacity?  They'll figure it out...
	// nah, better to send capacity; needed for channel refund
	msg = append(msg, capBytes...)
	_, err = RemoteCon.Write(msg)

	return
}

// QChanDescHandler takes in a description of a multisig output.  It then
// saves it to the local db.
func QChanDescHandler(from [16]byte, descbytes []byte) {
	if len(descbytes) != 77 {
		fmt.Printf("got %d byte multiDesc, expect 77\n", len(descbytes))
		return
	}
	peerBytes := RemoteCon.RemotePub.SerializeCompressed()
	// make sure their pubkey is a pubkey
	theirPub, err := btcec.ParsePubKey(descbytes[36:69], btcec.S256())
	if err != nil {
		fmt.Printf("QChanDescHandler err %s", err.Error())
		return
	}
	// deserialize outpoint
	var opBytes [36]byte
	copy(opBytes[:], descbytes[:36])
	op := uspv.OutPointFromBytes(opBytes)
	amt := uspv.BtI64(descbytes[69:])

	// save to db
	// it should go into the next bucket and get the right key index.
	// but we can't actually check that.
	err = SCon.TS.SaveFundTx(op, amt, peerBytes, theirPub)
	if err != nil {
		fmt.Printf("QChanDescHandler err %s", err.Error())
		return
	}
	fmt.Printf("got multisig output %s amt %d\n", op.String(), amt)
	// before acking, add to bloom filter.  Otherwise we won't see it as
	// it doesn't involve our utxos / adrs.
	err = SCon.TS.RefilterLocal()
	if err != nil {
		fmt.Printf("QChanDescHandler err %s", err.Error())
		return
	}

	// ACK the multi address, which causes the funder to sign / broadcast
	// ACK is outpoint (36), that's all.
	msg := []byte{uwire.MSGID_MULTIACK}
	msg = append(msg, uspv.OutPointToBytes(*op)...)
	_, err = RemoteCon.Write(msg)
	return
}

// QChanAckHandler takes in an acknowledgement multisig description.
// when a multisig outpoint is ackd, that causes the funder to sign and broadcast.
func QChanAckHandler(from [16]byte, ackbytes []byte) {
	if len(ackbytes) != 36 {
		fmt.Printf("got %d byte multiAck, expect 36\n", len(ackbytes))
		return
	}
	peerBytes := RemoteCon.RemotePub.SerializeCompressed()
	// deserialize outpoint
	var opBytes [36]byte
	copy(opBytes[:], ackbytes)
	op := uspv.OutPointFromBytes(opBytes)
	// sign multi tx
	tx, err := SCon.TS.SignFundTx(op, peerBytes)
	if err != nil {
		fmt.Printf("QChanAckHandler err %s", err.Error())
		return
	}
	fmt.Printf("tx to broadcast: %s ", uspv.TxToString(tx))
	err = SCon.NewOutgoingTx(tx)
	if err != nil {
		fmt.Printf("QChanAckHandler err %s", err.Error())
		return
	}
	return
}
