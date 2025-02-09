package anidb

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"github.com/EliasFleckenstein03/go-anidb/udp"
	"io"
	"runtime"
)

// We still have the key and IV somewhere in memory...
// but it's better than plaintext.
type credentials struct {
	username []byte
	password []byte
	udpKey   []byte
}

func (c *credentials) shred() {
	if c != nil {
		io.ReadFull(rand.Reader, c.username)
		io.ReadFull(rand.Reader, c.password)
		io.ReadFull(rand.Reader, c.udpKey)
		c.username = nil
		c.password = nil
		c.udpKey = nil
	}
}

// Randomly generated on every execution
var aesKey []byte

func init() {
	aesKey = make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, aesKey); err != nil {
		panic(err)
	}
}

func crypt(plaintext string) []byte {
	p := []byte(plaintext)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		panic(err)
	}

	ciphertext := make([]byte, len(p)+aes.BlockSize)
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		panic(err)
	}

	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], p)

	return ciphertext
}

func decrypt(ciphertext []byte) string {
	if len(ciphertext) <= aes.BlockSize {
		return ""
	}
	p := make([]byte, len(ciphertext)-aes.BlockSize)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		panic(err)
	}

	stream := cipher.NewCTR(block, ciphertext[:aes.BlockSize])
	stream.XORKeyStream(p, ciphertext[aes.BlockSize:])

	return string(p)
}

func newCredentials(username, password, udpKey string) *credentials {
	return &credentials{
		username: crypt(username),
		password: crypt(password),
		udpKey:   crypt(udpKey),
	}
}

func (udp *udpWrap) ReAuth() udpapi.APIReply {
	if Banned() {
		return bannedReply
	}

	udp.credLock.Lock()
	defer udp.credLock.Unlock()

	if c := udp.credentials; c != nil {
		udp.logRequest(paramSet{cmd: "AUTH", params: paramMap{"user": decrypt(c.username)}})
		r := udp.AniDBUDP.Auth(
			decrypt(c.username),
			decrypt(c.password),
			decrypt(c.udpKey))
		udp.logReply(r)

		err := r.Error()

		if err != nil {
			switch r.Code() {
			// 555 -- banned
			// 601 -- server down, treat the same as a ban
			case 555, 601:
				setBanned()
			case 500: // bad credentials
				udp.credentials.shred()
				udp.credentials = nil
			case 503, 504: // client rejected
				panic(err)
			}
		}
		udp.connected = err == nil

		if udp.connected {
			if user := UserByName(decrypt(c.username)); user != nil {
				udp.user = user
			} else {
				// We can't use SendRecv here as it would deadlock
				ch := make(chan udpapi.APIReply, 1)
				udp.sendQueueCh <- paramSet{
					cmd:    "USER",
					params: paramMap{"user": decrypt(c.username)},
					ch:     ch,
				}
				reply := <-ch

				if reply != nil {
					uid, _ := parseUserReply(reply)
					udp.user = uid.User()
				}
			}
		}

		runtime.GC()
		return r
	}
	return &noauthAPIReply{}
}

// Saves the used credentials in the AniDB struct, to allow automatic
// re-authentication when needed; they are (properly) encrypted with a key that's
// uniquely generated every time the module is initialized.
func (adb *AniDB) SetCredentials(username, password, udpKey string) {
	adb.udp.credLock.Lock()
	defer adb.udp.credLock.Unlock()

	adb.udp.credentials.shred()
	adb.udp.credentials = newCredentials(username, password, udpKey)
}

// Authenticates to anidb's UDP API and, on success, stores the credentials using
// SetCredentials. If udpKey is not "", the communication with the server
// will be encrypted, but in the VERY weak ECB mode.
func (adb *AniDB) Auth(username, password, udpKey string) (err error) {
	defer runtime.GC() // any better way to clean the plaintexts?

	adb.udp.sendLock.Lock()
	defer adb.udp.sendLock.Unlock()

	if !Banned() {
		adb.SetCredentials(username, password, udpKey)
	}

	// ReAuth clears the credentials if they're bad
	return adb.udp.ReAuth().Error()
}

// Logs the user out and removes the credentials from the AniDB struct.
func (adb *AniDB) Logout() error {
	adb.udp.credLock.Lock()
	defer adb.udp.credLock.Unlock()

	adb.udp.credentials.shred()
	adb.udp.credentials = nil

	adb.udp.sendLock.Lock()
	defer adb.udp.sendLock.Unlock()

	adb.udp.user = nil

	if adb.udp.connected {
		adb.udp.connected = false
		adb.udp.logRequest(paramSet{cmd: "LOGOUT"})
		return adb.udp.Logout()
	}
	return nil
}
