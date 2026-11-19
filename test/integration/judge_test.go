//go:build integration

package integration

import (
	"net/http"
	"strings"
	"testing"

	v1 "github.com/autocheck-team/backend/pkg/api/autocheck/v1"
)

func TestVerdictsAndReports(t *testing.T) {
	// arrange
	f := setup(t)
	f.pro(t, f.student)
	problem := f.problem(t, "go1.27", "cpp20-gcc15", "python3.14", "java25")
	tests := []struct {
		language string
		sources  map[v1.Verdict]string
	}{
		{"go1.27", map[v1.Verdict]string{
			v1.Verdict_VERDICT_OK:                    `package main; import "fmt"; func main(){var a,b int;fmt.Scan(&a,&b);fmt.Println(a+b)}`,
			v1.Verdict_VERDICT_WRONG_ANSWER:          `package main; import "fmt"; func main(){fmt.Println(0)}`,
			v1.Verdict_VERDICT_COMPILATION_ERROR:     `package main; func main( {`,
			v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED:   `package main;func main(){for{}}`,
			v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED: `package main;func main(){var a [][]byte;for{b:=make([]byte,1<<20);for i:=range b{b[i]=1};a=append(a,b)}}`,
			v1.Verdict_VERDICT_RUNTIME_ERROR:         `package main;import "os";func main(){os.Exit(2)}`,
		}},
		{"cpp20-gcc15", map[v1.Verdict]string{
			v1.Verdict_VERDICT_OK:                    "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b;}",
			v1.Verdict_VERDICT_WRONG_ANSWER:          "#include <iostream>\nint main(){std::cout<<0;}",
			v1.Verdict_VERDICT_COMPILATION_ERROR:     "invalid C++",
			v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED:   `int main(){for(;;){asm volatile("");}}`,
			v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED: "#include <cstdlib>\n#include <cstring>\nint main(){for(;;){char* p=(char*)malloc(1048576);if(!p)return 2;memset(p,1,1048576);asm volatile(\"\"::\"r\"(p):\"memory\");}}",
			v1.Verdict_VERDICT_RUNTIME_ERROR:         `int main(){return 2;}`,
		}},
		{"python3.14", map[v1.Verdict]string{
			v1.Verdict_VERDICT_OK:                    `print(sum(map(int,input().split())))`,
			v1.Verdict_VERDICT_WRONG_ANSWER:          `print(0)`,
			v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED:   `while True: pass`,
			v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED: "a=[]\nwhile True: a.append(bytearray(1024*1024))",
			v1.Verdict_VERDICT_RUNTIME_ERROR:         `raise RuntimeError("test")`,
		}},
		{"java25", map[v1.Verdict]string{
			v1.Verdict_VERDICT_OK:                    `import java.util.*;public class Main{public static void main(String[] a){Scanner s=new Scanner(System.in);System.out.println(s.nextInt()+s.nextInt());}}`,
			v1.Verdict_VERDICT_WRONG_ANSWER:          `public class Main{public static void main(String[] a){System.out.println(0);}}`,
			v1.Verdict_VERDICT_COMPILATION_ERROR:     `public class Main{invalid}`,
			v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED:   `public class Main{public static void main(String[] a){while(true){}}}`,
			v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED: `import sun.misc.Unsafe;public class Main{public static void main(String[] a)throws Exception{var f=Unsafe.class.getDeclaredField("theUnsafe");f.setAccessible(true);Unsafe u=(Unsafe)f.get(null);while(true){long p=u.allocateMemory(1024*1024);u.setMemory(p,1024*1024,(byte)1);}}}`,
			v1.Verdict_VERDICT_RUNTIME_ERROR:         `public class Main{public static void main(String[] a){System.exit(2);}}`,
		}},
	}
	for _, language := range tests {
		t.Run(language.language, func(t *testing.T) {
			for _, verdict := range []v1.Verdict{v1.Verdict_VERDICT_OK, v1.Verdict_VERDICT_WRONG_ANSWER, v1.Verdict_VERDICT_COMPILATION_ERROR, v1.Verdict_VERDICT_TIME_LIMIT_EXCEEDED, v1.Verdict_VERDICT_MEMORY_LIMIT_EXCEEDED, v1.Verdict_VERDICT_RUNTIME_ERROR} {
				source, ok := language.sources[verdict]
				if !ok {
					continue
				}
				t.Run(strings.ToLower(verdict.String()), func(t *testing.T) {
					// act
					sub := finished(t, f.student, problem, language.language, source)
					// assert
					if sub.GetVerdict() != verdict {
						t.Fatalf("verdict=%s want=%s compile=%s", sub.GetVerdict(), verdict, sub.GetCompileLog())
					}
					var report v1.GetSubmissionReportResponse
					call(t, f.student, http.MethodGet, "/v1/submissions/"+sub.GetId()+"/report", nil, &report)
					if report.GetVerdict() != verdict {
						t.Fatal("report verdict differs from watch")
					}
					if verdict == v1.Verdict_VERDICT_COMPILATION_ERROR && report.GetCompileLog() == "" {
						t.Fatal("missing compiler diagnostics")
					}
					if verdict == v1.Verdict_VERDICT_WRONG_ANSWER {
						if len(report.GetTestRuns()) != 2 {
							t.Fatal("expected open and closed test reports")
						}
						for _, run := range report.GetTestRuns() {
							if run.GetIsSample() {
								if run.GetDiff() == "" || run.GetInput().GetText() != "2 3\n" || run.GetExpectedOutput().GetText() != "5\n" || !strings.Contains(run.GetActualOutput().GetText(), "0") {
									t.Fatal("open test missing input, expected, actual or diff")
								}
							} else if run.GetInput() != nil || run.GetExpectedOutput() != nil || run.GetActualOutput() != nil || run.GetDiff() != "" {
								t.Fatal("closed test data leaked")
							}
						}
					}
				})
			}
		})
	}
	var testsVisible v1.ListTestCasesResponse
	call(t, f.student, http.MethodGet, "/v1/problems/"+problem+"/test-cases", nil, &testsVisible)
	if len(testsVisible.GetTestCases()) != 1 || !testsVisible.GetTestCases()[0].GetIsSample() {
		t.Fatal("student can see closed test")
	}
}
