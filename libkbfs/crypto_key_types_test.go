// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"testing"

	"github.com/keybase/client/go/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type kidContainerType interface {
	makeZero() interface{}
	makeFromKID(kid keybase1.KID) interface{}
	decode(codec *CodecMsgpack, data []byte) (interface{}, error)
}

// Make sure the kid container type encodes and decodes properly with
// minimal overhead.
func testKidContainerTypeEncodeDecode(t *testing.T, kt kidContainerType) {
	codec := NewCodecMsgpack()
	kidBytes := []byte{1}
	k := kt.makeFromKID(keybase1.KIDFromSlice(kidBytes))

	encodedK, err := codec.Encode(k)
	require.NoError(t, err)

	// See
	// https://github.com/msgpack/msgpack/blob/master/spec.md#formats-bin
	// for why there are two bytes of overhead.
	const overhead = 2
	assert.Equal(t, len(kidBytes)+overhead, len(encodedK))

	k2, err := kt.decode(codec, encodedK)
	require.NoError(t, err)

	assert.Equal(t, k, k2)
}

// Make sure the zero value for the kid container type encodes and
// decodes properly.
func testKidContainerTypeEncodeDecodeZero(t *testing.T, kt kidContainerType) {
	codec := NewCodecMsgpack()
	zeroValue := kt.makeZero()
	encodedK, err := codec.Encode(zeroValue)
	require.NoError(t, err)

	expectedEncodedK := []byte{0xc0}
	assert.Equal(t, expectedEncodedK, encodedK)

	k, err := kt.decode(codec, encodedK)
	require.NoError(t, err)

	assert.Equal(t, zeroValue, k)
}

type verifyingKeyType struct{}

func (verifyingKeyType) makeZero() interface{} {
	return IFCERFTVerifyingKey{}
}

func (verifyingKeyType) makeFromKID(kid keybase1.KID) interface{} {
	return IFCERFTMakeVerifyingKey(kid)
}

func (verifyingKeyType) decode(codec *CodecMsgpack, data []byte) (interface{}, error) {
	k := IFCERFTVerifyingKey{}
	err := codec.Decode(data, &k)
	return k, err
}

// Make sure VerifyingKey encodes and decodes properly with minimal overhead.
func TestVerifyingKeyEncodeDecode(t *testing.T) {
	testKidContainerTypeEncodeDecode(t, verifyingKeyType{})
}

// Make sure the zero VerifyingKey value encodes and decodes properly.
func TestVerifyingKeyEncodeDecodeZero(t *testing.T) {
	testKidContainerTypeEncodeDecodeZero(t, verifyingKeyType{})
}

type byte32ContainerType interface {
	makeZero() interface{}
	makeFromData(data [32]byte) interface{}
}

func testByte32ContainerEncodeDecode(t *testing.T, bt byte32ContainerType) {
	codec := NewCodecMsgpack()
	k := bt.makeFromData([32]byte{1, 2, 3, 4})

	encodedK, err := codec.Encode(k)
	require.NoError(t, err)

	// See
	// https://github.com/msgpack/msgpack/blob/master/spec.md#formats-bin
	// for why there are two bytes of overhead.
	const overhead = 2
	assert.Equal(t, 32+overhead, len(encodedK))

	k2 := bt.makeZero()
	err = codec.Decode(encodedK, &k2)
	require.NoError(t, err)

	assert.Equal(t, k, k2)
}

type tlfPrivateKeyType struct{}

func (tlfPrivateKeyType) makeZero() interface{} {
	return IFCERFTTLFPrivateKey{}
}

func (tlfPrivateKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFPrivateKey(data)
}

// Make sure TLFPrivateKey encodes and decodes properly with minimal
// overhead.
func TestTLFPrivateKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfPrivateKeyType{})
}

type tlfPublicKeyType struct{}

func (tlfPublicKeyType) makeZero() interface{} {
	return IFCERFTTLFPublicKey{}
}

func (tlfPublicKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFPublicKey(data)
}

// Make sure TLFPublicKey encodes and decodes properly with minimal
// overhead.
func TestTLFPublicKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfPublicKeyType{})
}

type tlfEphemeralPrivateKeyType struct{}

func (tlfEphemeralPrivateKeyType) makeZero() interface{} {
	return IFCERFTTLFEphemeralPrivateKey{}
}

func (tlfEphemeralPrivateKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFEphemeralPrivateKey(data)
}

// Make sure TLFEphemeralPrivateKey encodes and decodes properly with minimal
// overhead.
func TestTLFEphemeralPrivateKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfEphemeralPrivateKeyType{})
}

type cryptPublicKeyType struct{}

func (cryptPublicKeyType) makeZero() interface{} {
	return IFCERFTCryptPublicKey{}
}

func (cryptPublicKeyType) makeFromKID(kid keybase1.KID) interface{} {
	return IFCERFTMakeCryptPublicKey(kid)
}

func (cryptPublicKeyType) decode(codec *CodecMsgpack, data []byte) (interface{}, error) {
	k := IFCERFTCryptPublicKey{}
	err := codec.Decode(data, &k)
	return k, err
}

// Make sure CryptPublicKey encodes and decodes properly with minimal
// overhead.
func TestCryptPublicKeyEncodeDecode(t *testing.T) {
	testKidContainerTypeEncodeDecode(t, cryptPublicKeyType{})
}

// Make sure the zero CryptPublicKey value encodes and decodes
// properly.
func TestCryptPublicKeyEncodeDecodeZero(t *testing.T) {
	testKidContainerTypeEncodeDecodeZero(t, cryptPublicKeyType{})
}

type tlfEphemeralPublicKeyType struct{}

func (tlfEphemeralPublicKeyType) makeZero() interface{} {
	return IFCERFTTLFEphemeralPublicKey{}
}

func (tlfEphemeralPublicKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFEphemeralPublicKey(data)
}

// Make sure TLFEphemeralPublicKey encodes and decodes properly with minimal
// overhead.
func TestTLFEphemeralPublicKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfEphemeralPublicKeyType{})
}

type tlfCryptKeyServerHalfType struct{}

func (tlfCryptKeyServerHalfType) makeZero() interface{} {
	return IFCERFTTLFCryptKeyServerHalf{}
}

func (tlfCryptKeyServerHalfType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFCryptKeyServerHalf(data)
}

// Make sure TLFCryptKeyServerHalf encodes and decodes properly with
// minimal overhead.
func TestTLFCryptKeyServerHalfEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfCryptKeyServerHalfType{})
}

type tlfCryptKeyClientHalfType struct{}

func (tlfCryptKeyClientHalfType) makeZero() interface{} {
	return IFCERFTTLFCryptKeyClientHalf{}
}

func (tlfCryptKeyClientHalfType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFCryptKeyClientHalf(data)
}

// Make sure TLFCryptKeyClientHalf encodes and decodes properly with
// minimal overhead.
func TestTLFCryptKeyClientHalfEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfCryptKeyClientHalfType{})
}

type tlfCryptKeyType struct{}

func (tlfCryptKeyType) makeZero() interface{} {
	return IFCERFTTLFCryptKey{}
}

func (tlfCryptKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFCryptKey(data)
}

// Make sure TLFCryptKey encodes and decodes properly with minimal
// overhead.
func TestTLFCryptKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, tlfCryptKeyType{})
}

type blockCryptKeyServerHalfType struct{}

func (blockCryptKeyServerHalfType) makeZero() interface{} {
	return IFCERFTTLFCryptKey{}
}

func (blockCryptKeyServerHalfType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFCryptKey(data)
}

// Make sure BlockCryptKeyServerHalf encodes and decodes properly with
// minimal overhead.
func TestBlockCryptKeyServerHalfEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, blockCryptKeyServerHalfType{})
}

type blockCryptKeyType struct{}

func (blockCryptKeyType) makeZero() interface{} {
	return IFCERFTTLFCryptKey{}
}

func (blockCryptKeyType) makeFromData(data [32]byte) interface{} {
	return IFCERFTMakeTLFCryptKey(data)
}

// Make sure BlockCryptKey encodes and decodes properly with minimal
// overhead.
func TestBlockCryptKeyEncodeDecode(t *testing.T) {
	testByte32ContainerEncodeDecode(t, blockCryptKeyType{})
}
