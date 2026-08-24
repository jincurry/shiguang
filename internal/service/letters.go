package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"shiguang/internal/store"
)

// LetterDTO 是一封信的 API 表示。
type LetterDTO struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	Sender    string  `json:"sender"`
	DeliverAt string  `json:"deliver_at"`
	ReadAt    *string `json:"read_at"`
	// Pending 为真表示还没到投递时间。前台列表里不会出现这样的信，
	// 管理端靠它区分「已投递」与「等着投」——不能 omitempty，
	// 否则 false 时字段消失，前端分不清是 false 还是压根没这个字段。
	Pending bool `json:"pending"`
}

// LetterInput 是写信/改信的入参，字段为空表示不改。
type LetterInput struct {
	Title     *string `json:"title"`
	Body      *string `json:"body"`
	Sender    *string `json:"sender"`
	DeliverAt *string `json:"deliver_at"`
}

const maxLetterBody = 8000

func (s *Service) letterDTO(l *store.Letter, now string) *LetterDTO {
	return &LetterDTO{
		ID: l.ID, Title: l.Title, Body: l.Body, Sender: l.Sender,
		DeliverAt: l.DeliverAt, ReadAt: l.ReadAt,
		Pending: l.DeliverAt > now,
	}
}

func validateLetter(title, body, sender, deliverAt string) error {
	if strings.TrimSpace(title) == "" {
		return Validationf("信总得有个标题")
	}
	if len([]rune(title)) > 120 {
		return Validationf("标题不能超过 120 字")
	}
	if strings.TrimSpace(body) == "" {
		return Validationf("信的内容不能为空")
	}
	if len([]rune(body)) > maxLetterBody {
		return Validationf("信的内容不能超过 %d 字", maxLetterBody)
	}
	if len([]rune(sender)) > 60 {
		return Validationf("落款不能超过 60 字")
	}
	if _, err := time.Parse(store.TimeFormat, deliverAt); err != nil {
		return Validationf("投递时间格式不对")
	}
	return nil
}

// ParseDeliverAt 把前端来的时间收拾成存储格式。
// 接受完整时间戳、"YYYY-MM-DD HH:MM"、"YYYY-MM-DDTHH:MM" 与纯日期（当天 0 点）。
func ParseDeliverAt(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return store.Now(), nil // 不填就是立刻投递
	}
	for _, layout := range []string{
		store.TimeFormat, "2006-01-02T15:04:05Z", "2006-01-02T15:04",
		"2006-01-02 15:04", "2006-01-02",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return store.FormatTime(t), nil
		}
	}
	return "", Validationf("投递时间要写成 YYYY-MM-DD 或 YYYY-MM-DD HH:MM")
}

// CreateLetter 写一封信投进信箱。
func (s *Service) CreateLetter(ctx context.Context, in LetterInput) (*LetterDTO, error) {
	title, body, sender := "", "", ""
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		body = *in.Body
	}
	if in.Sender != nil {
		sender = strings.TrimSpace(*in.Sender)
	}
	deliverAt := store.Now()
	if in.DeliverAt != nil {
		d, err := ParseDeliverAt(*in.DeliverAt)
		if err != nil {
			return nil, err
		}
		deliverAt = d
	}
	if err := validateLetter(title, body, sender, deliverAt); err != nil {
		return nil, err
	}
	now := store.Now()
	l := &store.Letter{
		ID: NewULID(), Title: title, Body: body, Sender: sender,
		DeliverAt: deliverAt, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateLetter(ctx, l); err != nil {
		return nil, err
	}
	return s.letterDTO(l, now), nil
}

// Letters 列信。includePending 仅管理端为真；前台一律只看已投递的。
func (s *Service) Letters(ctx context.Context, includePending bool) ([]*LetterDTO, error) {
	now := store.Now()
	until := now
	if includePending {
		until = "" // 不加时间条件 = 连未投递的一起取
	}
	rows, err := s.st.Letters(ctx, until, 200)
	if err != nil {
		return nil, err
	}
	out := make([]*LetterDTO, 0, len(rows))
	for _, l := range rows {
		out = append(out, s.letterDTO(l, now))
	}
	return out, nil
}

// GetLetter 读一封信。markRead 为真时记下首次读取时刻（前台读走这条）。
func (s *Service) GetLetter(ctx context.Context, id string, includePending, markRead bool) (*LetterDTO, error) {
	now := store.Now()
	until := now
	if includePending {
		until = ""
	}
	l, err := s.st.GetLetter(ctx, id, until)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if markRead && l.ReadAt == nil {
		if err := s.st.MarkLetterRead(ctx, id, now); err != nil {
			return nil, err
		}
		l.ReadAt = &now
	}
	return s.letterDTO(l, now), nil
}

// PatchLetter 改信。
func (s *Service) PatchLetter(ctx context.Context, id string, in LetterInput) (*LetterDTO, error) {
	l, err := s.st.GetLetter(ctx, id, "")
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	title, body, sender, deliverAt := l.Title, l.Body, l.Sender, l.DeliverAt
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Body != nil {
		body = *in.Body
	}
	if in.Sender != nil {
		sender = strings.TrimSpace(*in.Sender)
	}
	if in.DeliverAt != nil {
		d, err := ParseDeliverAt(*in.DeliverAt)
		if err != nil {
			return nil, err
		}
		deliverAt = d
	}
	if err := validateLetter(title, body, sender, deliverAt); err != nil {
		return nil, err
	}
	now := store.Now()
	if err := s.st.UpdateLetter(ctx, id, title, body, sender, deliverAt, now); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetLetter(ctx, id, true, false)
}

// DeleteLetter 收回一封信。
func (s *Service) DeleteLetter(ctx context.Context, id string) error {
	err := s.st.DeleteLetter(ctx, id, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
