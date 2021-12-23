// Package storage implements a encrypted storage mechanism
// for interactsh external interaction data.
package storage

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/karlseguin/ccache/v2"
	"github.com/klauspost/compress/zlib"
	"github.com/pkg/errors"
)

// Storage is an storage for interactsh interaction data as well
// as correlation-id -> rsa-public-key data.
type Storage struct {
	cache       *ccache.Cache
	evictionTTL time.Duration
}

// CorrelationData is the data for a correlation-id.
type CorrelationData struct {
	// data contains data for a correlation-id in AES encrypted json format.
	Data []string `json:"data"`
	// dataMutex is a mutex for the data slice.
	dataMutex *sync.Mutex
	// secretkey is a secret key for original user verification
	token string
}

type CacheMetrics struct {
	Sessions int `json:"active-session"`
	Dropped  int `json:"evicted-session"`
}

func (s *Storage) GetCacheMetrics() *CacheMetrics {
	return &CacheMetrics{
		Sessions: s.cache.ItemCount(),
		Dropped:  s.cache.GetDropped(),
	}
}

// GetInteractions returns the uncompressed interactions for a correlation-id
func (c *CorrelationData) GetInteractions() []string {
	c.dataMutex.Lock()
	data := c.Data
	c.Data = make([]string, 0)
	c.dataMutex.Unlock()

	// Decompress the data and return a new slice
	if len(data) == 0 {
		return []string{}
	}

	buf := new(strings.Builder)
	results := make([]string, len(data))

	var reader io.ReadCloser
	for i, item := range data {
		var err error

		if reader == nil {
			reader, err = zlib.NewReader(strings.NewReader(item))
		} else {
			err = reader.(zlib.Resetter).Reset(strings.NewReader(item), nil)
		}
		if err != nil {
			continue
		}
		if _, err := io.Copy(buf, reader); err != nil {
			buf.Reset()
			continue
		}
		results[i] = buf.String()
		buf.Reset()
	}
	if reader != nil {
		_ = reader.Close()
	}
	return results
}

const defaultCacheMaxSize = 1000000

// New creates a new storage instance for interactsh data.
func New(evictionTTL time.Duration) *Storage {
	return &Storage{cache: ccache.New(ccache.Configure().MaxSize(defaultCacheMaxSize)), evictionTTL: evictionTTL}
}

// SetIDPublicKey sets the correlation ID and publicKey into the cache for further operations.
func (s *Storage) SetIDPublicKey(sessionID, token string) error {
	// If we already have this correlation ID, return.
	if s.cache.Get(sessionID) != nil {
		return errors.New("session-id provided is invalid")
	}

	data := &CorrelationData{
		Data:  make([]string, 0),
		token: token,
	}
	s.cache.Set(sessionID, data, s.evictionTTL)
	return nil
}

func (s *Storage) SetID(ID string) error {
	data := &CorrelationData{
		Data:      make([]string, 0),
		dataMutex: &sync.Mutex{},
	}
	s.cache.Set(ID, data, s.evictionTTL)
	return nil
}

// AddInteraction adds an interaction data to the correlation ID after encrypting
// it with Public Key for the provided correlation ID.
func (s *Storage) AddInteraction(sessionID string, data []byte) error {
	item := s.cache.Get(sessionID)
	if item == nil {
		return errors.New("could not get session-id from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return errors.New("invalid session-id cache value found")
	}

	// ct, err := aesEncrypt(value.aesKey, data)
	// if err != nil {
	// 	return errors.Wrap(err, "could not encrypt event data")
	// }
	value.dataMutex.Lock()
	value.Data = append(value.Data, string(data))
	value.dataMutex.Unlock()
	return nil
}

// AddInteractionWithId adds an interaction data to the id bucket
func (s *Storage) AddInteractionWithId(id string, data []byte) error {
	item := s.cache.Get(id)
	if item == nil {
		return errors.New("could not get session-id from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return errors.New("invalid session-id cache value found")
	}

	// Gzip compress to save memory for storage
	buffer := &bytes.Buffer{}

	gz := zippers.Get().(*zlib.Writer)
	defer zippers.Put(gz)
	gz.Reset(buffer)

	if _, err := gz.Write(data); err != nil {
		_ = gz.Close()
		return err
	}
	_ = gz.Close()

	value.dataMutex.Lock()
	value.Data = append(value.Data, buffer.String())
	value.dataMutex.Unlock()
	return nil
}

// GetInteractions returns the interactions for a correlationID and removes
// it from the storage. It also returns AES Encrypted Key for the IDs.
func (s *Storage) GetInteractions(correlationID, secret string) ([]string, string, error) {
	item := s.cache.Get(correlationID)
	if item == nil {
		return nil, "", errors.New("could not get correlation-id from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return nil, "", errors.New("invalid correlation-id cache value found")
	}
	// if !strings.EqualFold(value.secretKey, secret) {
	// 	return nil, "", errors.New("invalid secret key passed for user")
	// }
	data := value.GetInteractions()
	return data, "", nil // 3rd option was value.AESKey
}

// GetInteractions returns the interactions for a id and empty the cache
func (s *Storage) GetInteractionsWithId(id string) ([]string, error) {
	item := s.cache.Get(id)
	if item == nil {
		return nil, errors.New("could not get id from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return nil, errors.New("invalid id cache value found")
	}
	data := value.GetInteractions()
	return data, nil
}

// RemoveID removes data for a correlation ID and data related to it.
func (s *Storage) RemoveID(sessionID, token string) error {
	item := s.cache.Get(sessionID)
	if item == nil {
		return errors.New("could not get session-id from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return errors.New("invalid session-id cache value found")
	}
	// if !strings.EqualFold(value.secretKey, secret) {
	// 	return errors.New("invalid secret key passed for deregister")
	// }
	value.dataMutex.Lock()
	value.Data = nil
	value.dataMutex.Unlock()
	s.cache.Delete(sessionID)
	return nil
}

// parseB64RSAPublicKeyFromPEM parses a base64 encoded rsa pem to a public key structure
func parseB64RSAPublicKeyFromPEM(pubPEM string) (*rsa.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(pubPEM)
	if err != nil {
		return nil, err
	}

	block, _ := pem.Decode(decoded)
	if block == nil {
		return nil, errors.New("failed to parse PEM block containing the key")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}

	switch pub := pub.(type) {
	case *rsa.PublicKey:
		return pub, nil
	default:
		break // fall through
	}
	return nil, errors.New("Key type is not RSA")
}

var zippers = sync.Pool{New: func() interface{} {
	return zlib.NewWriter(nil)
}}

// aesEncrypt encrypts a message using AES and puts IV at the beginning of ciphertext.
func aesEncrypt(key []byte, message []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	// It's common to put IV at the beginning of the ciphertext.
	cipherText := make([]byte, aes.BlockSize+len(message))
	iv := cipherText[:aes.BlockSize]
	if _, err = io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(cipherText[aes.BlockSize:], message)

	encMessage := make([]byte, base64.StdEncoding.EncodedLen(len(cipherText)))
	base64.StdEncoding.Encode(encMessage, cipherText)

	// Gzip compress to save memory for storage
	buffer := &bytes.Buffer{}

	gz := zippers.Get().(*zlib.Writer)
	defer zippers.Put(gz)
	gz.Reset(buffer)

	if _, err := gz.Write(encMessage); err != nil {
		_ = gz.Close()
		return "", err
	}
	_ = gz.Close()

	return buffer.String(), nil
}

// GetCacheItem returns an item as is
func (s *Storage) GetCacheItem(token string) (*CorrelationData, error) {
	item := s.cache.Get(token)
	if item == nil {
		return nil, errors.New("could not get token from cache")
	}
	value, ok := item.Value().(*CorrelationData)
	if !ok {
		return nil, errors.New("invalid token cache value found")
	}
	return value, nil
}
