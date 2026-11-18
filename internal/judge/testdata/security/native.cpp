#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <string>
#include <sys/mount.h>
#include <sys/ptrace.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <unistd.h>

int main() {
#ifdef FORK_BOMB
  std::ifstream limit("/sys/fs/cgroup/pids.max");
  if (!limit.is_open()) limit.open("/sys/fs/cgroup/pids/pids.max");
  std::string pids;
  if (!(limit >> pids) || pids != "256") return 4;
  for (int count = 0; count < 4096; ++count) {
    pid_t child = fork();
    if (child == 0) {
      sleep(60);
      _exit(0);
    }
    if (child < 0) {
      if (errno != EAGAIN || count == 0 || count >= 256) return 3;
      std::fputs("PIDS_LIMIT\n", stderr);
      return 2;
    }
  }
  return 3;
#else
  if (getuid() != 65534 || geteuid() != 65534 || getgid() != 65534) return 10;
  if (setuid(0) != -1 || errno != EPERM) return 11;
  if (setgid(0) != -1 || errno != EPERM) return 12;
  if (mount("none", "/tmp", "tmpfs", 0, nullptr) != -1 || errno != EPERM) return 13;
  if (ptrace(PTRACE_ATTACH, 1, nullptr, nullptr) != -1) {
    std::fputs("PTRACE_ALLOWED\n", stderr);
    return 14;
  }
  if (errno != EPERM) return 14;
  char source = 'x', destination = 0;
  iovec local{&destination, 1}, remote{&source, 1};
  if (process_vm_readv(getpid(), &local, 1, &remote, 1, 0) != -1 || errno != EPERM) return 19;
  if (process_vm_writev(getpid(), &local, 1, &remote, 1, 0) != -1 || errno != EPERM) return 20;
  std::ifstream status("/proc/self/status");
  std::string line;
  int checked = 0;
  while (std::getline(status, line)) {
    if (line.rfind("Cap", 0) == 0) {
      if (line.substr(line.find(':') + 1).find_first_not_of("\t 0") != std::string::npos) return 15;
      ++checked;
    }
    if (line.rfind("NoNewPrivs:", 0) == 0) {
      if (line.find('1') == std::string::npos) return 16;
      ++checked;
    }
    if (line.rfind("Seccomp:", 0) == 0) {
      if (line.find('2') == std::string::npos) return 17;
      ++checked;
    }
  }
  if (checked != 7) return 18;
  std::puts("5");
  return 0;
#endif
}
