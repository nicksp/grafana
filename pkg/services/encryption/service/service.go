package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/usagestats"
	"github.com/grafana/grafana/pkg/services/encryption"
	"github.com/grafana/grafana/pkg/setting"
)

const (
	encryptionAlgorithmDelimiter = '*'

	securitySection            = "security.encryption"
	encryptionAlgorithmKey     = "algorithm"
	defaultEncryptionAlgorithm = encryption.AesCfb
)

// Service must not be used for encryption.
// Use secrets.Service implementing envelope encryption instead.
type Service struct {
	log log.Logger

	settingsProvider setting.Provider
	usageMetrics     usagestats.Service

	ciphers   map[string]encryption.Cipher
	deciphers map[string]encryption.Decipher
}

func ProvideEncryptionService(
	provider encryption.Provider,
	usageMetrics usagestats.Service,
	settingsProvider setting.Provider,
) (*Service, error) {
	s := &Service{
		log: log.New("encryption"),

		ciphers:   provider.ProvideCiphers(),
		deciphers: provider.ProvideDeciphers(),

		usageMetrics:     usageMetrics,
		settingsProvider: settingsProvider,
	}

	algorithm := s.settingsProvider.
		KeyValue(securitySection, encryptionAlgorithmKey).
		MustString(defaultEncryptionAlgorithm)

	if err := s.checkEncryptionAlgorithm(algorithm); err != nil {
		return nil, err
	}

	settingsProvider.RegisterReloadHandler(securitySection, s)

	s.registerUsageMetrics()

	return s, nil
}

func (s *Service) checkEncryptionAlgorithm(algorithm string) error {
	var err error
	defer func() {
		if err != nil {
			s.log.Error("Wrong security encryption configuration", "algorithm", algorithm, "error", err)
		}
	}()

	if _, ok := s.ciphers[algorithm]; !ok {
		err = errors.New("no cipher registered for encryption algorithm configured")
		return err
	}

	if _, ok := s.deciphers[algorithm]; !ok {
		err = errors.New("no cipher registered for encryption algorithm configured")
		return err
	}

	return nil
}

func (s *Service) registerUsageMetrics() {
	s.usageMetrics.RegisterMetricsFunc(func(context.Context) (map[string]interface{}, error) {
		algorithm := s.settingsProvider.
			KeyValue(securitySection, encryptionAlgorithmKey).
			MustString(defaultEncryptionAlgorithm)

		return map[string]interface{}{
			fmt.Sprintf("stats.encryption.%s.count", algorithm): 1,
		}, nil
	})
}

func (s *Service) Decrypt(ctx context.Context, payload []byte, secret string) ([]byte, error) {
	var err error
	defer func() {
		if err != nil {
			s.log.Error("Decryption failed", "error", err)
		}
	}()

	var (
		algorithm string
		toDecrypt []byte
	)
	algorithm, toDecrypt, err = deriveEncryptionAlgorithm(payload)
	if err != nil {
		return nil, err
	}

	decipher, ok := s.deciphers[algorithm]
	if !ok {
		err = fmt.Errorf("no decipher available for algorithm '%s'", algorithm)
		return nil, err
	}

	var decrypted []byte
	decrypted, err = decipher.Decrypt(ctx, toDecrypt, secret)

	return decrypted, err
}

func deriveEncryptionAlgorithm(payload []byte) (string, []byte, error) {
	if len(payload) == 0 {
		return "", nil, fmt.Errorf("unable to derive encryption algorithm")
	}

	if payload[0] != encryptionAlgorithmDelimiter {
		return encryption.AesCfb, payload, nil // backwards compatibility
	}

	payload = payload[1:]
	algorithmDelimiterIdx := bytes.Index(payload, []byte{encryptionAlgorithmDelimiter})
	if algorithmDelimiterIdx == -1 {
		return encryption.AesCfb, payload, nil // backwards compatibility
	}

	algorithmB64 := payload[:algorithmDelimiterIdx]
	payload = payload[algorithmDelimiterIdx+1:]

	algorithm := make([]byte, base64.RawStdEncoding.DecodedLen(len(algorithmB64)))

	_, err := base64.RawStdEncoding.Decode(algorithm, algorithmB64)
	if err != nil {
		return "", nil, err
	}

	return string(algorithm), payload, nil
}

func (s *Service) Encrypt(ctx context.Context, payload []byte, secret string) ([]byte, error) {
	var err error
	defer func() {
		if err != nil {
			s.log.Error("Encryption failed", "error", err)
		}
	}()

	algorithm := s.settingsProvider.
		KeyValue(securitySection, encryptionAlgorithmKey).
		MustString(defaultEncryptionAlgorithm)

	cipher, ok := s.ciphers[algorithm]
	if !ok {
		err = fmt.Errorf("no cipher available for algorithm '%s'", algorithm)
		return nil, err
	}

	var encrypted []byte
	encrypted, err = cipher.Encrypt(ctx, payload, secret)

	prefix := make([]byte, base64.RawStdEncoding.EncodedLen(len([]byte(algorithm)))+2)
	base64.RawStdEncoding.Encode(prefix[1:], []byte(algorithm))
	prefix[0] = encryptionAlgorithmDelimiter
	prefix[len(prefix)-1] = encryptionAlgorithmDelimiter

	ciphertext := make([]byte, len(prefix)+len(encrypted))
	copy(ciphertext, prefix)
	copy(ciphertext[len(prefix):], encrypted)

	return ciphertext, nil
}

func (s *Service) EncryptJsonData(ctx context.Context, kv map[string]string, secret string) (map[string][]byte, error) {
	encrypted := make(map[string][]byte)
	for key, value := range kv {
		encryptedData, err := s.Encrypt(ctx, []byte(value), secret)
		if err != nil {
			return nil, err
		}

		encrypted[key] = encryptedData
	}
	return encrypted, nil
}

func (s *Service) DecryptJsonData(ctx context.Context, sjd map[string][]byte, secret string) (map[string]string, error) {
	decrypted := make(map[string]string)
	for key, data := range sjd {
		decryptedData, err := s.Decrypt(ctx, data, secret)
		if err != nil {
			return nil, err
		}

		decrypted[key] = string(decryptedData)
	}
	return decrypted, nil
}

func (s *Service) GetDecryptedValue(ctx context.Context, sjd map[string][]byte, key, fallback, secret string) string {
	if value, ok := sjd[key]; ok {
		decryptedData, err := s.Decrypt(ctx, value, secret)
		if err != nil {
			return fallback
		}

		return string(decryptedData)
	}

	return fallback
}

func (s *Service) Validate(section setting.Section) error {
	s.log.Debug("Validating encryption config")

	algorithm := section.KeyValue(encryptionAlgorithmKey).
		MustString(defaultEncryptionAlgorithm)

	if err := s.checkEncryptionAlgorithm(algorithm); err != nil {
		return err
	}

	return nil
}

func (s *Service) Reload(_ setting.Section) error {
	return nil
}
