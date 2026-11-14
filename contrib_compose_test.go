package skift_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"go.yaml.in/yaml/v3"
)

// deployCompose is the compose file a person runs skift from, and the
// template of the copy a release carries.
// docs/adrs/011-releases-are-immutable-and-attested.md, decision 7.
const deployCompose = "contrib/docker-compose.yaml"

// deployProject keeps a stack the specs start apart from one a person runs
// from the same file, whose project is skift.
const deployProject = "skift-contrib-spec"

// composeFile is the part of a compose file the specs reason about.
type composeFile struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image     string   `yaml:"image"`
		Command   any      `yaml:"command"`
		Ports     []any    `yaml:"ports"`
		Volumes   []string `yaml:"volumes"`
		Restart   string   `yaml:"restart"`
		DependsOn map[string]struct {
			Condition string `yaml:"condition"`
		} `yaml:"depends_on"`
		Healthcheck struct {
			Test []string `yaml:"test"`
		} `yaml:"healthcheck"`
	} `yaml:"services"`
}

func loadCompose(path string) composeFile {
	GinkgoHelper()
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	var c composeFile
	Expect(yaml.Unmarshal(raw, &c)).To(Succeed(), path)
	return c
}

// interpolated matches a variable compose substitutes, and not the $${...}
// that reaches a container's shell as written.
var interpolated = regexp.MustCompile(`(?:^|[^$])\$\{([A-Za-z_][A-Za-z0-9_]*)`)

// deployVariables names every variable the deployment compose file reads.
func deployVariables() []string {
	GinkgoHelper()
	raw, err := os.ReadFile(deployCompose)
	Expect(err).NotTo(HaveOccurred())
	seen := map[string]bool{}
	var names []string
	for _, match := range interpolated.FindAllStringSubmatch(string(raw), -1) {
		if !seen[match[1]] {
			seen[match[1]] = true
			names = append(names, match[1])
		}
	}
	return names
}

// envFile writes the given settings where compose can read them, and
// returns the path.
func envFile(settings map[string]string) string {
	GinkgoHelper()
	var b strings.Builder
	for name, value := range settings {
		fmt.Fprintf(&b, "%s=%s\n", name, value)
	}
	path := filepath.Join(GinkgoT().TempDir(), "skift.env")
	Expect(os.WriteFile(path, []byte(b.String()), 0o600)).To(Succeed())
	return path
}

// deployComposeIn runs podman compose on the deployment compose file as it
// stands in dir, with the settings in an env file and none from the
// environment the specs run in: a variable set there would stand in for
// one the env file leaves out.
func deployComposeIn(dir, settings string, timeout time.Duration, args ...string) *gexec.Session {
	GinkgoHelper()
	cmd := exec.Command("podman", append([]string{"compose", "--env-file", settings, "-f", deployCompose}, args...)...)
	cmd.Dir = dir
	read := deployVariables()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if inherited(kv) && !strings.HasPrefix(name, "COMPOSE_") && !slices.Contains(read, name) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "COMPOSE_PROJECT_NAME="+deployProject, "PODMAN_COMPOSE_WARNING_LOGS=false")
	session, err := gexec.Start(cmd, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())
	return session.Wait(timeout)
}

// output is everything a finished command wrote.
func output(session *gexec.Session) string {
	return string(session.Out.Contents()) + string(session.Err.Contents())
}

var _ = Describe("The deployment compose file", Label("unit"), func() {
	required := map[string]string{
		"SKIFT_IMAGE":       "ghcr.io/steading-ai/skift:9.9.9",
		"POSTGRES_USER":     "skift",
		"POSTGRES_PASSWORD": "spec",
		"POSTGRES_DB":       "skift",
	}

	DescribeTable("A missing secret fails before anything starts",
		func(missing string) {
			By("given an env file without " + missing)
			settings := map[string]string{}
			for name, value := range required {
				if name != missing {
					settings[name] = value
				}
			}

			By("when docker compose config runs on contrib/docker-compose.yaml")
			session := deployComposeIn(".", envFile(settings), time.Minute, "config")

			By("then it exits non-zero")
			Expect(session).NotTo(gexec.Exit(0))
			By("and the message names " + missing)
			Expect(string(session.Err.Contents())).To(ContainSubstring("required variable " + missing))
		},
		Entry("POSTGRES_PASSWORD", "POSTGRES_PASSWORD"),
		Entry("POSTGRES_USER", "POSTGRES_USER"),
		Entry("POSTGRES_DB", "POSTGRES_DB"),
		Entry("SKIFT_IMAGE", "SKIFT_IMAGE"),
	)

	It("needs nothing but the required settings", func() {
		Expect(deployComposeIn(".", envFile(required), time.Minute, "config")).To(gexec.Exit(0))
	})

	It("has a commented entry in skift.env.example for every variable it reads, and no secret", func() {
		raw, err := os.ReadFile("contrib/skift.env.example")
		Expect(err).NotTo(HaveOccurred())

		example := map[string]string{}
		commented := true
		for _, line := range strings.Split(string(raw), "\n") {
			name, value, isSetting := strings.Cut(line, "=")
			switch {
			case strings.HasPrefix(line, "#"):
				commented = true
			case isSetting:
				Expect(commented).To(BeTrue(), "%s has a comment above it", name)
				example[name] = value
				commented = false
			default:
				commented = false
			}
		}

		var names []string
		for name := range example {
			names = append(names, name)
		}
		Expect(names).To(ConsistOf(deployVariables()))
		for _, secret := range []string{"POSTGRES_PASSWORD", "TS_AUTHKEY"} {
			Expect(example).To(HaveKeyWithValue(secret, ""))
		}
	})

	It("runs a durable PostgreSQL, pinned by digest and published nowhere", func() {
		deploy := loadCompose(deployCompose)
		Expect(deploy.Name).To(Equal("skift"))

		postgres := deploy.Services["postgres"]
		Expect(postgres.Image).To(Equal(loadCompose("compose.yaml").Services["postgres"].Image),
			"the development database and the deployment run one PostgreSQL")
		Expect(postgres.Image).To(MatchRegexp(`^docker\.io/library/postgres:18@sha256:[0-9a-f]{64}$`))
		Expect(postgres.Command).To(BeNil(), "the image's own command: no durability traded away")
		Expect(postgres.Ports).To(BeEmpty())
		Expect(postgres.Volumes).To(ConsistOf("pgdata:/var/lib/postgresql"))
		Expect(strings.Join(postgres.Healthcheck.Test, " ")).To(ContainSubstring("pg_isready -h 127.0.0.1"))
	})

	It("runs skift serve from the image, once PostgreSQL is healthy, with its socket and state in volumes", func() {
		skift := loadCompose(deployCompose).Services["skift"]

		Expect(skift.Image).To(HavePrefix("${SKIFT_IMAGE:?"))
		Expect(skift.Command).To(Equal([]any{"skift", "serve"}))
		Expect(skift.DependsOn).To(HaveKey("postgres"))
		Expect(skift.DependsOn["postgres"].Condition).To(Equal("service_healthy"))
		Expect(skift.Restart).To(Equal("unless-stopped"))
		Expect(skift.Ports).To(BeEmpty())
		// A named volume, never a path on the host: a socket caller's uid
		// means nothing outside the container.
		Expect(skift.Volumes).To(ConsistOf("run:/run/skift", "state:/var/lib/skift"))
	})

	Describe("the copy a release carries", func() {
		const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

		var dir string

		BeforeEach(func() {
			dir = checkout()
		})

		It("The release copy pins the image by digest", func() {
			By("given IMAGE is ghcr.io/steading-ai/skift:9.9.9@sha256 followed by 64 hex characters")
			image := "ghcr.io/steading-ai/skift:9.9.9@" + digest

			By("when make renders dist/release/docker-compose.yaml")
			runIn(dir, time.Minute, "make", "dist/release/docker-compose.yaml", "IMAGE="+image)

			By("then the rendered file differs from the template only on the image line")
			template, err := os.ReadFile(filepath.Join(dir, deployCompose))
			Expect(err).NotTo(HaveOccurred())
			rendered, err := os.ReadFile(filepath.Join(dir, "dist", "release", "docker-compose.yaml"))
			Expect(err).NotTo(HaveOccurred())

			was, is := strings.Split(string(template), "\n"), strings.Split(string(rendered), "\n")
			Expect(is).To(HaveLen(len(was)))
			var changed []int
			for i := range was {
				if was[i] != is[i] {
					changed = append(changed, i)
				}
			}
			Expect(changed).To(HaveLen(1))
			Expect(was[changed[0]]).To(HavePrefix("    image: ${SKIFT_IMAGE"))

			By("and that line names IMAGE exactly")
			Expect(is[changed[0]]).To(Equal("    image: " + image))
		})

		It("pins a release candidate's image too", func() {
			image := "ghcr.io/steading-ai/skift:9.9.9-rc.1@" + digest
			runIn(dir, time.Minute, "make", "dist/release/docker-compose.yaml", "IMAGE="+image)

			rendered := loadCompose(filepath.Join(dir, "dist", "release", "docker-compose.yaml"))
			Expect(rendered.Services["skift"].Image).To(Equal(image))
		})

		DescribeTable("refuses an IMAGE that is not a released image pinned by digest",
			func(image string) {
				session := sessionIn(dir, time.Minute, "make", "dist/release/docker-compose.yaml", "IMAGE="+image)

				Expect(session).NotTo(gexec.Exit(0))
				Expect(string(session.Err.Contents())).To(ContainSubstring("IMAGE must be"))
				Expect(filepath.Join(dir, "dist", "release", "docker-compose.yaml")).NotTo(BeAnExistingFile())
			},
			Entry("no IMAGE at all", ""),
			Entry("a tag with no digest", "ghcr.io/steading-ai/skift:9.9.9"),
			Entry("a digest with no tag", "ghcr.io/steading-ai/skift@"+digest),
			Entry("a moving tag", "ghcr.io/steading-ai/skift:latest@"+digest),
			Entry("a short digest", "ghcr.io/steading-ai/skift:9.9.9@"+digest[:len(digest)-1]),
			Entry("another repository", "ghcr.io/steading-ai/other:9.9.9@"+digest),
		)
	})
})

// The stack the deployment compose file describes, run for real. Serial,
// because it compiles skift and builds an image before it starts two
// containers.
var _ = Describe("The deployment stack", Label("db", "compose", "slow"), Serial, Ordered, func() {
	const image = "localhost/skift-contrib-spec:9.9.9"

	var (
		dir     string
		compose func(timeout time.Duration, args ...string) *gexec.Session
	)

	BeforeAll(func() {
		dir = checkout()
		runIn(dir, 10*time.Minute, "make", "dist/skift-linux-"+runtime.GOARCH, "VERSION=9.9.9")

		By("given an env file with SKIFT_IMAGE naming a locally built image")
		settings := envFile(map[string]string{
			"SKIFT_IMAGE":       image,
			"POSTGRES_USER":     "skift",
			"POSTGRES_PASSWORD": "spec",
			"POSTGRES_DB":       "skift",
		})
		compose = func(timeout time.Duration, args ...string) *gexec.Session {
			GinkgoHelper()
			return deployComposeIn(dir, settings, timeout, args...)
		}

		// Built in the engine compose will run it on, which only sees its
		// own images: podman, unless DOCKER_HOST names another, as CI's
		// does with the runner's Docker.
		engine, build := "podman", []string{"build"}
		if host := os.Getenv("DOCKER_HOST"); host != "" && !strings.Contains(host, "podman") {
			engine, build = "docker", []string{"buildx", "build", "--load"}
		}
		runIn(dir, 10*time.Minute, engine, append(build,
			"--build-arg", "TARGETARCH="+runtime.GOARCH,
			"--build-arg", "REVISION=spec",
			"-f", "packaging/Containerfile",
			"-t", image, ".")...)
		DeferCleanup(func() {
			runIn(dir, time.Minute, engine, "rmi", "-f", image)
		})

		Expect(compose(2*time.Minute, "down", "--volumes", "--remove-orphans")).To(gexec.Exit(0))
		DeferCleanup(func() {
			Expect(compose(2*time.Minute, "down", "--volumes", "--remove-orphans")).To(gexec.Exit(0))
		})
	})

	It("The stack serves from the image", func() {
		logs := func() string {
			return output(compose(time.Minute, "logs", "--no-color", "skift"))
		}

		By("when docker compose up -d runs and the skift service has started")
		Expect(compose(5*time.Minute, "up", "--detach")).To(gexec.Exit(0))

		By(`then the skift log contains "skift serve starting"`)
		Eventually(logs).WithTimeout(time.Minute).WithPolling(500 * time.Millisecond).
			Should(ContainSubstring("skift serve starting"))
		running := compose(time.Minute, "ps", "--services", "--status", "running")
		Expect(running).To(gexec.Exit(0))
		Expect(strings.Fields(string(running.Out.Contents()))).To(ConsistOf("postgres", "skift"))

		By("and after bootstrapping uid 65532, skift project list run inside the container succeeds")
		// Bootstrap needs the schema, which serve brings up once the
		// database answers.
		Eventually(logs).WithTimeout(time.Minute).WithPolling(500 * time.Millisecond).
			Should(ContainSubstring("database reachable; ready"))
		Expect(compose(2*time.Minute, "run", "--rm", "-T", "skift",
			"skift", "admin", "bootstrap", "65532", "spec")).To(gexec.Exit(0))
		Expect(compose(time.Minute, "exec", "-T", "skift", "skift", "project", "list")).To(gexec.Exit(0))
	})
})
