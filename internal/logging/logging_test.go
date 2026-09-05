package logging_test

import (
	"bytes"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ivy/dagger/internal/logging"
)

var _ = Describe("Structured logging", Label("unit"), func() {
	var buf *bytes.Buffer

	BeforeEach(func() { buf = &bytes.Buffer{} })

	It("writes plain key=value records outside the journal", func() {
		log := slog.New(logging.NewHandler(buf, slog.LevelInfo, false))
		log.Info("listening", "socket", "/run/dagger/dg.sock")
		Expect(buf.String()).To(SatisfyAll(
			HavePrefix("time="),
			ContainSubstring(`msg=listening socket=/run/dagger/dg.sock`),
		))
	})

	It("prefixes each record with its journald priority under the journal", func() {
		log := slog.New(logging.NewHandler(buf, slog.LevelDebug, true))
		log.Debug("d")
		log.Info("i")
		log.Warn("w")
		log.Error("e")
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		Expect(lines).To(HaveLen(4))
		Expect(lines[0]).To(HavePrefix("<7>"))
		Expect(lines[1]).To(HavePrefix("<6>"))
		Expect(lines[2]).To(HavePrefix("<4>"))
		Expect(lines[3]).To(HavePrefix("<3>"))
	})

	It("keeps the priority prefix through WithAttrs and WithGroup", func() {
		log := slog.New(logging.NewHandler(buf, slog.LevelInfo, true)).With("component", "server").WithGroup("req")
		log.Warn("slow", "ms", 900)
		Expect(buf.String()).To(SatisfyAll(
			HavePrefix("<4>"),
			ContainSubstring("component=server"),
			ContainSubstring("req.ms=900"),
		))
	})

	It("honours the level", func() {
		log := slog.New(logging.NewHandler(buf, slog.LevelWarn, true))
		log.Info("quiet")
		Expect(buf.String()).To(BeEmpty())
	})

	It("detects the journal from JOURNAL_STREAM", func() {
		GinkgoT().Setenv("JOURNAL_STREAM", "")
		Expect(logging.UnderJournal()).To(BeFalse())
		GinkgoT().Setenv("JOURNAL_STREAM", "8:12345")
		Expect(logging.UnderJournal()).To(BeTrue())
	})
})
