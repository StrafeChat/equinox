package devices

import (
	"context"
)

type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// RegisterDevice stores public keys for a device. Client generates keys; server stores only public parts.
func (s *Service) RegisterDevice(ctx context.Context, userID int64, in *RegisterDeviceInput) error {
	d := &DeviceKeys{
		DeviceID:           in.DeviceID,
		IdentityKeyPublic:  in.IdentityKey,
		SignedPrekeyPublic: in.SignedPrekey,
		SignedPrekeySig:    in.SignedPrekeySig,
		SignedPrekeyID:     in.SignedPrekeyID,
		RegistrationID:     in.RegistrationID,
	}
	if err := s.repo.UpsertDevice(ctx, userID, d); err != nil {
		return err
	}
	if len(in.OneTimePrekeys) > 0 {
		return s.repo.AddOneTimePrekeys(ctx, userID, in.DeviceID, in.OneTimePrekeys)
	}
	return nil
}

// GetPrekeyBundle returns a bundle for X3DH session init. Consumes one one-time prekey if available.
func (s *Service) GetPrekeyBundle(ctx context.Context, userID, deviceID int64) (*PrekeyBundle, error) {
	d, err := s.repo.GetDevice(ctx, userID, deviceID)
	if err != nil || d == nil {
		return nil, err
	}
	b := &PrekeyBundle{
		IdentityKey:     d.IdentityKeyPublic,
		SignedPrekey:    d.SignedPrekeyPublic,
		SignedPrekeyID:  d.SignedPrekeyID,
		SignedPrekeySig: d.SignedPrekeySig,
		RegistrationID:  d.RegistrationID,
	}
	otp, err := s.repo.TakeOneTimePrekey(ctx, userID, deviceID)
	if err != nil {
		return nil, err
	}
	if otp != nil {
		b.OneTimePrekey = &struct {
			KeyID     int    `json:"key_id"`
			PublicKey string `json:"public_key"`
		}{
			KeyID:     otp.KeyID,
			PublicKey: otp.PublicKey,
		}
	}
	return b, nil
}
