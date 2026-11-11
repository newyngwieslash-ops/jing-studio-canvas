package app

import (
	"mime/multipart"

	"yingce/backend/internal/model"
)

func (s *Service) UpdateProfileAvatar(actor *model.User, header *multipart.FileHeader) (*AuthUser, error) {
	res, err := s.authDomain().UpdateProfileAvatar(actor, header)
	if err == nil && actor != nil {
		s.invalidateUserAuthCache(actor.ID)
	}
	return res, err
}

func (s *Service) UseDefaultProfileAvatar(actor *model.User) (*AuthUser, error) {
	res, err := s.authDomain().UseDefaultProfileAvatar(actor)
	if err == nil && actor != nil {
		s.invalidateUserAuthCache(actor.ID)
	}
	return res, err
}

func (s *Service) SelectDefaultProfileAvatar(actor *model.User, preset string) (*AuthUser, error) {
	res, err := s.authDomain().SelectDefaultProfileAvatar(actor, preset)
	if err == nil && actor != nil {
		s.invalidateUserAuthCache(actor.ID)
	}
	return res, err
}

func (s *Service) ProfileAvatarContent(actor *model.User, targetUserID string) ([]byte, error) {
	return s.authDomain().ProfileAvatarContent(actor, targetUserID)
}
