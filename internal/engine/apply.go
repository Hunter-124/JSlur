package engine

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"autoapply/internal/config"
	"autoapply/internal/store"
)

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

func slug(s string) string {
	s = slugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	if s == "" {
		s = "job"
	}
	return strings.ToLower(s)
}

// exportApplication writes the tailored materials and job details to a folder
// under dir, returning the folder path.
func exportApplication(dir string, job store.Job, app store.Application) (string, error) {
	folder := filepath.Join(dir, fmt.Sprintf("%s-%s-%s", slug(job.Company), slug(job.Title), job.ID))
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return "", err
	}

	var meta strings.Builder
	fmt.Fprintf(&meta, "%s\n%s\n\n", job.Title, job.Company)
	fmt.Fprintf(&meta, "Location: %s\n", job.Location)
	if job.Salary != "" {
		fmt.Fprintf(&meta, "Salary: %s\n", job.Salary)
	}
	fmt.Fprintf(&meta, "Source: %s\nURL: %s\n", job.Source, job.URL)
	if job.ApplyURL != "" {
		fmt.Fprintf(&meta, "Apply at (company site): %s\n", job.ApplyURL)
	}
	if job.CompanyURL != "" {
		fmt.Fprintf(&meta, "Company site: %s\n", job.CompanyURL)
	}
	if job.ApplyEmail != "" {
		fmt.Fprintf(&meta, "Apply email: %s\n", job.ApplyEmail)
	}
	fmt.Fprintf(&meta, "\nMatch score: %d/100\n%s\n\n---- JOB DESCRIPTION ----\n\n%s\n",
		app.MatchScore, app.MatchReason, job.Description)

	files := map[string]string{
		"resume.md":        app.Resume,
		"cover_letter.txt": app.CoverLetter,
		"job.txt":          meta.String(),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(folder, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return folder, nil
}

// emailApplication sends the cover letter as the body with the resume attached.
func emailApplication(smtpCfg config.SMTPConfig, to string, job store.Job, app store.Application) error {
	if smtpCfg.Host == "" {
		return fmt.Errorf("SMTP host not configured")
	}
	if to == "" {
		return fmt.Errorf("job has no apply email address")
	}
	from := smtpCfg.From
	if from == "" {
		from = smtpCfg.Username
	}
	if from == "" {
		return fmt.Errorf("SMTP 'from' address not configured")
	}

	subject := fmt.Sprintf("Application for %s", job.Title)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Top-level message headers.
	headers := map[string]string{
		"From":         from,
		"To":           to,
		"Subject":      subject,
		"MIME-Version": "1.0",
		"Content-Type": "multipart/mixed; boundary=" + mw.Boundary(),
	}
	var head bytes.Buffer
	for k, v := range headers {
		fmt.Fprintf(&head, "%s: %s\r\n", k, v)
	}
	head.WriteString("\r\n")

	// Body part (cover letter)
	bodyPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type": {"text/plain; charset=utf-8"},
	})
	if err != nil {
		return err
	}
	body := app.CoverLetter
	if body == "" {
		body = "Please find my application attached."
	}
	bodyPart.Write([]byte(body))

	// Attachment (resume)
	att, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":        {"text/markdown; charset=utf-8"},
		"Content-Disposition": {`attachment; filename="resume.md"`},
	})
	if err != nil {
		return err
	}
	att.Write([]byte(app.Resume))
	mw.Close()

	msg := append(head.Bytes(), buf.Bytes()...)

	addr := fmt.Sprintf("%s:%d", smtpCfg.Host, smtpCfg.Port)
	var auth smtp.Auth
	if smtpCfg.Username != "" {
		auth = smtp.PlainAuth("", smtpCfg.Username, smtpCfg.Password, smtpCfg.Host)
	}
	return smtp.SendMail(addr, auth, from, []string{to}, msg)
}
