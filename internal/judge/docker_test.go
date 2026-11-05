package judge

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/autocheck-team/backend/internal/sandbox"
)

// TestDockerLanguages проверяет реальные тулчейны в той же изоляции, что и воркер.
func TestDockerLanguages(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = cli.Ping(ctx, client.PingOptions{})
	cancel()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	type sources struct{ ok, wa, ce, tl, ml, re string }
	tests := []struct {
		language Language
		source   sources
	}{
		{Language{ID: "go1.27", Image: "golang:1.27-alpine", SourceFile: "main.go", CompileCmd: []string{"go", "build", "-o", "solution", "main.go"}, RunCmd: []string{"./solution"}, CompileEnv: []string{"GOCACHE=/work/box/.cache", "GOTMPDIR=/work/box", "CGO_ENABLED=0"}}, sources{
			ok: `package main; import "fmt"; func main(){ var a,b int; fmt.Scan(&a,&b);fmt.Println(a+b) }`,
			wa: `package main; import "fmt"; func main(){fmt.Println(0)}`,
			ce: `package main; func main( {`,
			tl: `package main; func main(){for {}}`,
			ml: `package main; func main(){var a [][]byte; for { b:=make([]byte,1<<20);for i:=range b {b[i]=1};a=append(a,b)}}`,
			re: `package main; import "os"; func main(){os.Exit(2)}`,
		}},
		{Language{ID: "cpp20-gcc15", Image: "gcc:15", SourceFile: "main.cpp", CompileCmd: []string{"g++", "-std=c++20", "-O2", "-o", "solution", "main.cpp"}, RunCmd: []string{"./solution"}, CompileEnv: []string{"TMPDIR=/work/box"}}, sources{
			ok: `#include <iostream>
int main(){int a,b;std::cin>>a>>b;std::cout<<a+b;}`,
			wa: `#include <iostream>
int main(){std::cout<<0;}`,
			ce: `invalid C++`,
			tl: `int main(){for(;;){asm volatile("");}}`,
			ml: `#include <cstdlib>
#include <cstring>
int main(){for(;;){char* p=(char*)malloc(1048576);if(!p)return 2;memset(p,1,1048576);asm volatile(""::"r"(p):"memory");}}`,
			re: `int main(){return 2;}`,
		}},
		{Language{ID: "python3.14", Image: "python:3.14-alpine", SourceFile: "main.py", RunCmd: []string{"python3", "main.py"}}, sources{
			ok: `print(sum(map(int,input().split())))`, wa: `print(0)`, tl: "while True: pass", ml: "a=[]\nwhile True: a.append(bytearray(1024*1024))", re: `raise RuntimeError("test")`,
		}},
		{Language{ID: "java25", Image: "eclipse-temurin:25-jdk-alpine", SourceFile: "Main.java", CompileCmd: []string{"javac", "-J-XX:+UseSerialGC", "-J-XX:ActiveProcessorCount=1", "Main.java"}, RunCmd: []string{"java", "-XX:+UseSerialGC", "-XX:ActiveProcessorCount=1", "-XX:MaxRAMPercentage=50", "-Xss512k", "Main"}}, sources{
			ok: `import java.util.*; public class Main {public static void main(String[] a){Scanner s=new Scanner(System.in);System.out.println(s.nextInt()+s.nextInt());}}`,
			wa: `public class Main {public static void main(String[] a){System.out.println(0);}}`,
			ce: `public class Main { invalid }`,
			tl: `public class Main {public static void main(String[] a){while(true){}}}`,
			// Нативные выделения достигают cgroup-лимита, минуя Java heap exception.
			ml: `import sun.misc.Unsafe; public class Main {public static void main(String[] a) throws Exception {var f=Unsafe.class.getDeclaredField("theUnsafe");f.setAccessible(true);Unsafe u=(Unsafe)f.get(null);while(true){long p=u.allocateMemory(1024*1024);u.setMemory(p,1024*1024,(byte)1);}}}`,
			re: `public class Main {public static void main(String[] a){System.exit(2);}}`,
		}},
	}
	var pool *pgxpool.Pool
	if os.Getenv("DATABASE_URL") != "" {
		pool = judgeDB(t)
	}
	for _, tt := range tests {
		t.Run(tt.language.ID, func(t *testing.T) {
			// При наличии БД проверяем именно команды и env применённой миграции.
			if pool != nil {
				err := pool.QueryRow(t.Context(), `SELECT image,source_file,compile_cmd,run_cmd,compile_env FROM languages WHERE id=$1`, tt.language.ID).Scan(&tt.language.Image, &tt.language.SourceFile, &tt.language.CompileCmd, &tt.language.RunCmd, &tt.language.CompileEnv)
				if err != nil {
					t.Fatal(err)
				}
			}

			imageCtx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			defer cancel()
			if err := sb.EnsureImage(imageCtx, tt.language.Image); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct{ name, source string }{{"OK", tt.source.ok}, {"WA", tt.source.wa}, {"CE", tt.source.ce}, {"TL", tt.source.tl}, {"ML", tt.source.ml}, {"RE", tt.source.re}} {
				if tc.source == "" {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					repo, objects, _, _ := runnerFixture()
					repo.task.Tests = repo.task.Tests[:1]
					repo.task.Language = tt.language
					repo.task.Problem.TimeLimitMS = 1000
					repo.task.Problem.MemoryLimitKB = 128 * 1024
					objects.data["source"] = []byte(tc.source)
					out, err := NewRunner(repo, objects, Docker{Sandbox: sb}).Run(t.Context(), Job{SubmissionID: uuid.New()})
					if err != nil {
						t.Fatal(err)
					}
					if out.Verdict != tc.name {
						t.Fatalf("verdict=%s want=%s compile=%q runs=%+v", out.Verdict, tc.name, out.CompileLog, repo.runs)
					}
				})
			}
		})
	}
}

func TestDockerCustomCheckerIsolation(t *testing.T) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, err = cli.Ping(ctx, client.PingOptions{})
	cancel()
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	sb, err := sandbox.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	repo, objects, _, _ := runnerFixture()
	repo.task.Language = Language{Image: "python:3.14-alpine", SourceFile: "main.py", RunCmd: []string{"python3", "main.py"}}
	repo.task.Problem.Checker = "custom"
	key := "checker"
	repo.task.Problem.CheckerKey = &key
	objects.data["source"] = []byte("import os\nassert not os.path.exists('checker.py')\nassert not os.path.exists('expected')\nprint(sum(map(int,input().split())))\n")
	objects.data[key] = []byte("import sys\nfrom pathlib import Path\nassert Path(sys.argv[1]).read_text() == '2 3'\nsys.exit(0 if Path(sys.argv[2]).read_bytes().strip() == Path(sys.argv[3]).read_bytes().strip() else 1)\n")
	out, err := NewRunner(repo, objects, Docker{Sandbox: sb}).Run(t.Context(), Job{SubmissionID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Verdict != "OK" || len(repo.runs) != 2 {
		t.Fatalf("outcome=%+v runs=%+v", out, repo.runs)
	}
}
