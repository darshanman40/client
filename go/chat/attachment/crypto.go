// This is a construction for encrypting and signing a message, using a
// symmetric encryption key and a signing keypair, in a way that supports safe
// streaming decryption. We need this for chat attachments because we've chosen
// to use signing keys for authenticity in chat, and we don't want one
// participant to be able to modify another's attachment, even with an evil
// server's help. It's *almost* enough that we record the hash of the
// attachment along with the symmetric key used to encrypt it, but that by
// itself doesn't allow safe streaming decryption. Instead, we use this
// construction to sign each chunk of the attachment as we encrypt it. (Note
// that it's still possible for a sender with the server's help to modify their
// *own* attachments after the fact, if clients aren't checking the hash. This
// isn't perfect, but it's better than any participant being able to do it.)
//
// Seal inputs:
// - plaintext bytes (streaming is fine)
// - a crypto_secretbox symmetric key
// - a crypto_sign private key
// - a globally unique (with respect to these keys) 16-byte nonce
//
// Seal steps:
// 1) Chunk the message into exactly-one-megabyte (2^20 bytes) chunks, with
//    exactly one short chunk at the end, which might be zero bytes.
// 2) Compute the SHA512 hash of each plaintext chunk.
// 3) Concatenate the 16-byte nonce above with the 8-byte unsigned big-endian
//    chunk number, where the first chunk is zero. This is the 24-byte chunk
//    nonce.
// 4) Concatenate four things:
//    - "keybase chat attachment\0" (that's a null byte at the end)
//    - the encryption key (why?! read below)
//    - the chunk nonce from #3
//    - the hash from #2.
// 5) Sign the concatenation from #3, giving a detached 64-byte crypto_sign
//    signature.
// 6) Concatenate the signature from #4 + the plaintext chunk.
// 7) Encrypt the concatenation from #5 with the crypto_secretbox key and the
//    chunk nonce from #3.
// 8) Prepend the ciphertext from #7 with the byte 0xc6 + its own length as a
//    4-byte unsigned big-endian integer. (This encodes a MessagePack bin32
//    object.)
// 9) Concatenate all the bin32 objects from #8 into the output.
//
// Open inputs:
// - ciphertext bytes (streaming is fine)
// - the same crypto_secretbox symmetric key
// - the corresponding crypto_sign public key
// - the same nonce
//
// Open steps:
// 1) MessagePack unpack the input stream into a series of bin objects.
//    (Because the encoder is strict, it's also safe to assume a known
//    constant length for all chunks before the last one.)
// 2) Decrypt each binary chunk with the crypto_secretbox key and chunk
//    nonce as in seal step #7.
// 3) Split the chunk into a 64-byte signature and the following plaintext.
// 4) Hash that plaintext and make the concatenation from seal step #4.
// 5) Verify the signature against that concatenation.
// 6) Emit each verified plaintext chunk as output.
// 7) If we reach the end of the input without encountering a short chunk,
//    raise a truncation error.
// 8) If we've already encountered a short chunk, and we're fed more bytes,
//    raise an extra bytes error. (Implementations assuming constant chunk
//    length can't hit this condition. Input with extra bytes will get
//    mis-chunked and cause a decoding/unboxing/verifying error instead.)
//
// Design Notes:
//
// Combining signing and encryption is surprisingly tricky! See
// http://world.std.com/~dtd/sign_encrypt/sign_encrypt7.html for lots of
// details about the issues that come up. (Note that "encryption" in that
// paper refers mostly to RSA encryption like PGP's, which doesn't involve
// a sender key the way Diffie-Hellman / NaCl's crypto_box does. This makes
// me appreciate just how many problems the crypto_box construction is
// solving.)
//
// Many of these issues probably don't apply to chat attachments (yet?!),
// because recipients will know what keys to use ahead of time. But there
// are other places where we use signing+encryption that have different
// properties, and I want to be able to use this design as a reference. The
// short version of the problem is that both encrypt-then-sign and
// sign-then-encrypt have to worry about what happens when someone reuses
// the inner layer with a new outer layer.
//
// Encrypt-then-sign has a "sender impersonation" problem. The
// man-in-the-middle can re-sign an encrypted payload with their own key
// and claim authorship of the message. If the message itself contains
// secrets, like in an auth protocol for example, the MITM can fake knowing
// those secrets. (Also, encrypt-then-sign has the more obvious downside
// that encryption is hiding only the contents of a signature and not its
// author.)
//
// Sign-then-encrypt has a "surreptitious forwarding" problem. A recipient
// can re-encrypt the signed payload to another unintended recipient.
// Recipients must not rely on the encryption layer to mean that the sender
// intended the message for them. In fact PGP is vulnerable to this attack,
// unless the user/application understands the very subtle difference
// between "I can read this" and "this was written to me".
//
// So, simply using encryption and signing together isn't good enough! The
// paper linked above mentions a few different solutions, but in general
// the fix is that the inner layer needs to assert something about the
// outer layer, so that the outer layer can't be changed without the inner
// key. We could simply include the outer key verbatim inside the inner
// layer, but a better approach is to mix the outer key into the inner
// crypto, so that it's impossible to forget to check it.
//
// We prefer sign-then-encrypt, because hiding the author of the signature
// is a feature. That means the inner signing layer needs to assert the
// encryption key. We do this by including the encryption key as
// "associated data" that gets signed along with the plaintext. Since we
// already need to do that with a nonce and a chunk number, including the
// the encryption key is easy. We don't need to worry about whether the
// signature might leak the encryption key either, because the signature
// gets encrypted.
//
// Apart from these signing gymnastics, all the large-encrypted-message
// considerations from https://www.imperialviolet.org/2015/05/16/aeads.html
// apply here. Namely we use a chunk number to prevent reordering, and we
// use an empty chunk at the end to detect truncation. A globally unique
// nonce (for encryption *and* signing) prevents chunk swapping in between
// messages, and is required for encryption in any case. (It's expected
// that the chat client will pass in all zeroes for the nonce, because both
// keys are one-time-use. That's up to the client. G-d help us if we ever
// reuse those keys.) We also follow the "prefix signatures with an ASCII
// context string and a null byte" recommendation from
// https://www.ietf.org/mail-archive/web/tls/current/msg14734.html.

package attachment

import (
	"crypto/sha512"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/agl/ed25519"
	"golang.org/x/crypto/nacl/secretbox"
)

type AttachmentNonce *[AttachmentNonceSize]byte
type SecretboxKey *[SecretboxKeySize]byte
type SecretboxNonce *[SecretboxNonceSize]byte
type SignKey *[ed25519.PrivateKeySize]byte
type VerifyKey *[ed25519.PublicKeySize]byte

const AttachmentNonceSize = 16
const SecretboxKeySize = 32
const SecretboxNonceSize = 24
const SignaturePrefix = "keybase chat attachment\x00"
const PlaintextChunkLength = 1048576 // 2^20
const Bin32Tag = 0xc6
const Bin32Overhead = 5 // The bin32 tag plus 4 length bytes
const PacketLength = PlaintextChunkLength + ed25519.SignatureSize + secretbox.Overhead + Bin32Overhead

// ===================================
// single packet encoding and decoding
// ===================================

func makeChunkNonce(attachmentNonce AttachmentNonce, chunkNum uint64) SecretboxNonce {
	var ret [SecretboxNonceSize]byte
	copy(ret[0:16], attachmentNonce[:])
	var chunkNumBytes [8]byte
	binary.BigEndian.PutUint64(chunkNumBytes[:], chunkNum)
	copy(ret[16:24], chunkNumBytes[:])
	return &ret
}

func makeSignatureInput(plaintext []byte, encKey SecretboxKey, chunkNonce SecretboxNonce) []byte {
	chunkHash := sha512.Sum512(plaintext)
	var ret []byte
	ret = append(ret, SignaturePrefix...)
	ret = append(ret, encKey[:]...)
	ret = append(ret, chunkNonce[:]...)
	ret = append(ret, chunkHash[:]...)
	return ret
}

// When serializing ciphertext chunks, we always use the bin32 MessagePack
// format, regardless of the size of the chunk. That lets us predict the
// ciphertext size given the plaintext size.
func packCiphertext(ciphertext []byte) []byte {
	if len(ciphertext) > math.MaxUint32 {
		panic("ciphertext chunk is too long")
	}
	len32 := uint32(len(ciphertext))
	// A bin32 object is byte 0xc6 followed by a 32-bit big-endian length.
	packet := []byte{0xc6, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(packet[1:5], len32)
	packet = append(packet, ciphertext...)
	return packet
}

func getPacketLen(plaintextLen int) int {
	return plaintextLen + secretbox.Overhead + ed25519.SignatureSize + Bin32Overhead
}

func sealPacket(plaintext []byte, chunkNum uint64, encKey SecretboxKey, signKey SignKey, attachmentNonce AttachmentNonce) []byte {
	chunkNonce := makeChunkNonce(attachmentNonce, chunkNum)
	signatureInput := makeSignatureInput(plaintext, encKey, chunkNonce)
	signature := ed25519.Sign(signKey, signatureInput)
	signedChunk := append(signature[:], plaintext...)
	ciphertextChunk := secretbox.Seal(nil, signedChunk, chunkNonce, encKey)
	packet := packCiphertext(ciphertextChunk)
	return packet
}

func unpackCiphertext(packet []byte) ([]byte, error) {
	// This function could just chop off the first 5 bytes and be done with
	// it, but we do extra checks to be as strict as possible, so that
	// implementations that want to use an actual MessagePack decoder have
	// that option.
	if len(packet) < Bin32Overhead {
		return nil, NewAttachmentError(ShortMessagePackObject,
			"can't unpack bin object less than length 5, found %d", len(packet))
	}
	if packet[0] != Bin32Tag {
		return nil, NewAttachmentError(WrongMessagePackFormat,
			"expected tag byte 0xc6, found %#x", packet[0])
	}
	length := binary.BigEndian.Uint32(packet[1:5])
	if uint64(length+5) != uint64(len(packet)) {
		return nil, NewAttachmentError(WrongMessagePackLength,
			"encoded length doesn't match packet size: %d + 5 != %d", length, len(packet))
	}
	return packet[Bin32Overhead:len(packet)], nil
}

func openPacket(packet []byte, chunkNum uint64, encKey SecretboxKey, verifyKey VerifyKey, attachmentNonce AttachmentNonce) ([]byte, error) {
	ciphertext, err := unpackCiphertext(packet)
	if err != nil {
		return nil, err
	}
	chunkNonce := makeChunkNonce(attachmentNonce, chunkNum)
	signedChunk, secretboxValid := secretbox.Open(nil, ciphertext, chunkNonce, encKey)
	if !secretboxValid {
		return nil, NewAttachmentError(BadSecretbox, "secretbox failed to open")
	}
	// Avoid panicking on signatures that are too short.
	if len(signedChunk) < ed25519.SignatureSize {
		return nil, NewAttachmentError(ShortSignature, "signature too short")
	}
	var signature [ed25519.SignatureSize]byte
	copy(signature[:], signedChunk[0:ed25519.SignatureSize])
	plaintext := signedChunk[ed25519.SignatureSize:len(signedChunk)]
	signatureInput := makeSignatureInput(plaintext, encKey, chunkNonce)
	signatureValid := ed25519.Verify(verifyKey, signatureInput, &signature)
	if !signatureValid {
		return nil, NewAttachmentError(BadSignature, "signature failed to verify")
	}
	return plaintext, nil
}

// =============================
// streaming attachment encoding
// =============================

type AttachmentEncoder struct {
	encKey   SecretboxKey
	signKey  SignKey
	nonce    AttachmentNonce
	buf      []byte
	chunkNum uint64
}

func NewAttachmentEncoder(encKey SecretboxKey, signKey SignKey, nonce AttachmentNonce) *AttachmentEncoder {
	return &AttachmentEncoder{
		encKey:   encKey,
		signKey:  signKey,
		nonce:    nonce,
		buf:      nil,
		chunkNum: 0,
	}
}

func (e *AttachmentEncoder) sealOnePacket(plaintextLen int) []byte {
	// Note that this function handles the `plaintextLen == 0` case.
	if plaintextLen > len(e.buf) {
		panic("encoder tried to seal a packet that was too big")
	}
	plaintextChunk := e.buf[0:plaintextLen]
	packet := sealPacket(plaintextChunk, e.chunkNum, e.encKey, e.signKey, e.nonce)
	e.buf = e.buf[plaintextLen:len(e.buf)]
	e.chunkNum++
	return packet
}

// Write plaintext bytes into the encoder. If any output bytes are ready,
// return them. Callers must call Finish() when they're done, so that any
// remaining input bytes can be written out as a short (or empty) chunk.
// Otherwise you will both lose data and cause truncation errors on
// decoding.
func (e *AttachmentEncoder) Write(plaintext []byte) []byte {
	e.buf = append(e.buf, plaintext...)
	var output []byte
	// If buf is big enough to make new packets, make as many as we can.
	for len(e.buf) >= PlaintextChunkLength {
		packet := e.sealOnePacket(PlaintextChunkLength)
		output = append(output, packet...)
	}
	return output
}

// Write out any remaining buffered input bytes (possibly zero bytes) as a
// short chunk. This should only be called once, and after that you can't
// use this encoder again.
func (e *AttachmentEncoder) Finish() []byte {
	if len(e.buf) >= PlaintextChunkLength {
		panic("encoder buffer has more bytes than expected")
	}
	packet := e.sealOnePacket(len(e.buf))
	return packet
}

// =============================
// streaming attachment decoding
// =============================

type AttachmentDecoder struct {
	encKey    SecretboxKey
	verifyKey VerifyKey
	nonce     AttachmentNonce
	buf       []byte
	chunkNum  uint64
	err       error
}

func NewAttachmentDecoder(encKey SecretboxKey, verifyKey VerifyKey, nonce AttachmentNonce) *AttachmentDecoder {
	return &AttachmentDecoder{
		encKey:    encKey,
		verifyKey: verifyKey,
		nonce:     nonce,
		buf:       nil,
		chunkNum:  0,
		err:       nil,
	}
}

func (d *AttachmentDecoder) openOnePacket(packetLen int) ([]byte, error) {
	if packetLen > len(d.buf) {
		panic("decoder tried to open a packet that was too big")
	}
	packet := d.buf[0:packetLen]
	plaintext, err := openPacket(packet, d.chunkNum, d.encKey, d.verifyKey, d.nonce)
	if err != nil {
		return nil, err
	}
	d.buf = d.buf[packetLen:len(d.buf)]
	d.chunkNum++
	return plaintext, nil
}

// Write ciphertext bytes into the decoder. If any packets are ready to
// open, open them and either return their plaintext bytes as output or any
// error that comes up. Callers must call Finish() when they're done, to
// decode the final short packet and check for truncation. If Write ever
// returns an error, subsequent calls to Write will always return the same
// error.
func (d *AttachmentDecoder) Write(ciphertext []byte) ([]byte, error) {
	// If we've ever seen an error, just return that again.
	if d.err != nil {
		return nil, d.err
	}
	d.buf = append(d.buf, ciphertext...)
	// If buf is big enough to open new packets, open as many as we can.
	// We assume that every packet other than the last (which we handle in
	// Finish) is the same length, which makes this loop very simple.
	var output []byte
	for len(d.buf) >= PacketLength {
		var plaintext []byte
		plaintext, d.err = d.openOnePacket(PacketLength)
		if d.err != nil {
			return nil, d.err
		}
		output = append(output, plaintext...)
	}
	return output, nil
}

// Decode any remaining bytes as a short (or empty) packet. This produces
// the final bytes of the plaintext, and implicitly checks for truncation.
// This should only be called once, and after that you can't use this
// decoder again.
func (d *AttachmentDecoder) Finish() ([]byte, error) {
	// If we've ever seen an error, just return that again.
	if d.err != nil {
		return nil, d.err
	}
	if len(d.buf) >= PacketLength {
		panic("decoder buffer has more bytes than expected")
	}
	// If we've been truncated at a packet boundary, this open will fail on a
	// simple length check. If we've been truncated in the middle of a packet,
	// this open will fail to validate.
	var plaintext []byte
	plaintext, d.err = d.openOnePacket(len(d.buf))
	return plaintext, d.err
}

// =============================
// all-at-once wrapper functions
// =============================

func GetSealedSize(plaintextLen int) int {
	// All the full packets.
	fullChunks := plaintextLen / PlaintextChunkLength
	totalLen := fullChunks * getPacketLen(PlaintextChunkLength)
	// Maybe a partial packet.
	remainingPlaintext := plaintextLen % PlaintextChunkLength
	totalLen += getPacketLen(remainingPlaintext)
	// And finally, an empty packet.
	return totalLen
}

func SealWholeAttachment(plaintext []byte, encKey SecretboxKey, signKey SignKey, nonce AttachmentNonce) []byte {
	encoder := NewAttachmentEncoder(encKey, signKey, nonce)
	output := encoder.Write(plaintext)
	output = append(output, encoder.Finish()...)
	return output
}

func OpenWholeAttachment(sealed []byte, encKey SecretboxKey, verifyKey VerifyKey, nonce AttachmentNonce) ([]byte, error) {
	decoder := NewAttachmentDecoder(encKey, verifyKey, nonce)
	output, err := decoder.Write(sealed)
	if err != nil {
		return nil, err
	}
	moreOutput, err := decoder.Finish()
	if err != nil {
		return nil, err
	}
	return append(output, moreOutput...), nil
}

// ======
// errors
// ======

type AttachmentCryptoErrorType int

const (
	BadSecretbox AttachmentCryptoErrorType = iota
	ShortSignature
	BadSignature
	ShortMessagePackObject
	WrongMessagePackFormat
	WrongMessagePackLength
)

type AttachmentCryptoError struct {
	Type    AttachmentCryptoErrorType
	Message string
}

func NewAttachmentError(errorType AttachmentCryptoErrorType, message string, args ...interface{}) error {
	return AttachmentCryptoError{
		Type:    errorType,
		Message: fmt.Sprintf(message, args...),
	}
}

func (e AttachmentCryptoError) Error() string {
	return e.Message
}