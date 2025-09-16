package notification

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/restfront/logger"
	simplemail "github.com/xhit/go-simple-mail/v2"
)

type SMTPServer struct {
	Host     string
	Port     int
	Username string
	Password string
}

type MailService struct {
	config        *MailConfig
	logger        *logger.Logger
	queue         chan EmailMessage
	actionTimeout time.Duration
	closed        bool
	closeMutex    sync.Mutex
}

type MailConfig struct {
	PrimaryServer SMTPServer
	BackupServer  SMTPServer
	QueueSize     int
}

type EmailMessage struct {
	To      string
	Subject string
	Body    string
}

var defaultActionTimeout = 10 * time.Second

func NewMailService(config *MailConfig, logger *logger.Logger) *MailService {
	return &MailService{
		config:        config,
		logger:        logger,
		queue:         make(chan EmailMessage, config.QueueSize),
		actionTimeout: defaultActionTimeout,
	}
}

func (s *MailService) AddMessage(message EmailMessage) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Errorf("Паника в сервисе отправки email: %v", r)
			}
		}()

		s.closeMutex.Lock()
		isClosed := s.closed
		s.closeMutex.Unlock()

		if isClosed {
			s.logger.Warn("Попытка добавления email после закрытия очереди сообщений")
			return
		}

		timer := time.NewTimer(defaultActionTimeout)
		defer timer.Stop()

		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case s.queue <- message:
				return
			case <-timer.C:
				s.logger.Warn("Время добавления email в очередь истекло, сообщение не отправлено")
				return
			case <-ticker.C:
				s.logger.Warn("Очередь отправки email переполнена, ожидание освобождения очереди")
			}
		}
	}()
}

func (s *MailService) Start(ctx context.Context) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.logger.Errorf("Паника в сервисе отправки email: %v", r)
			}
		}()

		for message := range s.queue {
			s.sendEmailWithTimeout(ctx, message)
		}
	}()
}

func (s *MailService) sendEmailWithTimeout(ctx context.Context, message EmailMessage) {
	sendTimeout := 30 * time.Second

	timeoutCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	done := make(chan error)

	go func() {
		done <- s.sendEmail(message)
	}()

	select {
	case err := <-done:
		if err != nil {
			s.logger.Errorf("Ошибка при отправке email: %v", err)
		}
	case <-timeoutCtx.Done():
		s.logger.Warn("Время отправки email истекло, сообщение не отправлено")
	}
}

func (s *MailService) sendEmail(message EmailMessage) error {
	err := s.sendViaSMTP(s.config.PrimaryServer, message)
	if err != nil {
		errSend := fmt.Errorf("ошибка при отправке email через основной SMTP-сервер: %w", err)

		if s.config.BackupServer.Host == "" {
			return errSend
		}

		s.logger.Errorf("Ошибка при отправке email: %v", errSend)

		err = s.sendViaSMTP(s.config.BackupServer, message)
		if err != nil {
			return fmt.Errorf("ошибка при отправке email через резервный SMTP-сервер: %w", err)
		}
	}

	return nil
}

func (s *MailService) sendViaSMTP(smtp SMTPServer, message EmailMessage) error {
	server := simplemail.NewSMTPClient()

	server.Host = smtp.Host
	server.Port = smtp.Port
	server.Username = smtp.Username
	server.Password = smtp.Password
	server.KeepAlive = false

	switch server.Port {
	case 465:
		server.Encryption = simplemail.EncryptionSSLTLS
	case 587:
		server.Encryption = simplemail.EncryptionSTARTTLS
	default:
		server.Encryption = simplemail.EncryptionNone
	}

	smtpClient, err := server.Connect()
	if err != nil {
		return err
	}
	defer smtpClient.Close()

	email := simplemail.NewMSG()

	email.SetFrom(smtp.Username).
		AddTo(message.To).
		SetSubject(message.Subject).
		SetBody(simplemail.TextPlain, message.Body)

	if email.Error != nil {
		return email.Error
	}

	err = email.Send(smtpClient)
	if err != nil {
		return err
	}

	return nil
}

func (s *MailService) Stop() {
	s.closeMutex.Lock()
	defer s.closeMutex.Unlock()

	if s.closed {
		return
	}

	s.closed = true
	close(s.queue)
	s.logger.Info("Сервис отправки email остановлен")
}
