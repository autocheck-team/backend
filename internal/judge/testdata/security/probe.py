import errno
import os
from pathlib import Path
import socket
import subprocess
import sys
import threading
import time


def process_limit():
    for path in ("/sys/fs/cgroup/pids.max", "/sys/fs/cgroup/pids/pids.max"):
        if Path(path).exists():
            assert Path(path).read_text().strip() == "256"
            return
    raise AssertionError("pids controller unavailable")


def denied_read(path):
    try:
        data = Path(path).read_bytes()
    except (PermissionError, FileNotFoundError, NotADirectoryError):
        return
    # Маскированный proc-файл может открываться как пустой /dev/null.
    assert path == "/proc/1/environ" and not data, path


if scenario == "fork_python":
    process_limit()
    children = 0
    try:
        for _ in range(4096):
            if os.fork() == 0:
                time.sleep(60)
                os._exit(0)
            children += 1
    except OSError as error:
        assert error.errno == errno.EAGAIN and 0 < children < 256
        print("PIDS_LIMIT", file=sys.stderr, flush=True)
        sys.exit(2)
    raise AssertionError("process limit did not fire")
elif scenario == "fork_shell":
    process_limit()
    os.execl("/bin/sh", "sh", "-c", "i=0; while [ $i -lt 4096 ]; do sleep 60 & i=$((i+1)); done; wait")
elif scenario == "threads":
    process_limit()
    children = 0
    try:
        for _ in range(4096):
            threading.Thread(target=time.sleep, args=(60,), daemon=True).start()
            children += 1
    except RuntimeError:
        assert 0 < children < 256
        print("THREAD_LIMIT", file=sys.stderr, flush=True)
        sys.exit(2)
    raise AssertionError("thread limit did not fire")
elif scenario.startswith("tcp_"):
    target = {
        "tcp_external": "1.1.1.1",
        "tcp_gateway": "172.17.0.1",
        "tcp_desktop_host": "192.168.65.254",
        "tcp_metadata": "169.254.169.254",
        "tcp_ipv6": "2606:4700:4700::1111",
    }[scenario]
    family = socket.AF_INET6 if ":" in target else socket.AF_INET
    with socket.socket(family, socket.SOCK_STREAM) as connection:
        connection.settimeout(0.5)
        try:
            connection.connect((target, 80))
        except OSError as error:
            assert error.errno in (errno.ENETUNREACH, errno.EHOSTUNREACH, errno.ETIMEDOUT, errno.EACCES), error
        else:
            raise AssertionError("network reachable")
elif scenario == "dns":
    for hostname in ("example.com", "host.docker.internal", "gateway.docker.internal"):
        try:
            result = subprocess.run(
                [sys.executable, "-c", "import socket,sys; socket.getaddrinfo(sys.argv[1],80)", hostname],
                timeout=2, capture_output=True,
            )
        except subprocess.TimeoutExpired:
            continue
        assert result.returncode != 0 and b"gaierror" in result.stderr, hostname
elif scenario == "files":
    for path in (
        "/etc/shadow", "/proc/1/environ", "/var/run/docker.sock", "/run/docker.sock",
        "/expected", "/work/box/expected", "/work/box/checker.py",
        "/proc/1/root/etc/shadow", "/var/lib/docker/volumes", "/work/box/neighbor-secret",
    ):
        denied_read(path)
    os.symlink("/etc/shadow", "shadow-link")
    denied_read("shadow-link")
    assert set(os.listdir("/work/box")) == {"main.py", "shadow-link"}
elif scenario == "write_rootfs":
    for path in ("/etc/pwned", "/work/pwned", "/sys/kernel/uevent_helper", "/proc/sys/kernel/hostname"):
        try:
            with open(path, "wb") as output:
                output.write(b"sandbox-test")
        except OSError as error:
            assert error.errno in (errno.EROFS, errno.EACCES, errno.EPERM, errno.ENOENT), (path, error)
        else:
            raise AssertionError(path)
    Path("writable").write_text("allowed")
elif scenario == "file_size":
    with open("big", "wb", buffering=0) as output:
        while True:
            output.write(b"x" * (1024 * 1024))
elif scenario == "workspace_full":
    storage = os.statvfs(".")
    assert storage.f_blocks * storage.f_frsize <= 256 * 1024 * 1024
    # Несколько файлов обходили RLIMIT_FSIZE и заполняли диск Docker.
    chunk = b"x" * (1024 * 1024)
    try:
        for index in range(40):
            with open(str(index), "wb", buffering=0) as output:
                for _ in range(8):
                    output.write(chunk)
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
        print("WORKSPACE_FULL", file=sys.stderr, flush=True)
        sys.exit(2)
    raise AssertionError("workspace is unbounded")
elif scenario == "inodes":
    assert os.statvfs(".").f_files <= 16384
    try:
        for index in range(20000):
            Path(f"empty-{index}").touch()
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
        print("INODE_LIMIT", file=sys.stderr, flush=True)
        sys.exit(2)
    raise AssertionError("inodes are unbounded")
elif scenario == "tmp_full":
    try:
        with open("/tmp/big", "wb", buffering=0) as output:
            for _ in range(32):
                output.write(b"x" * (1024 * 1024))
    except OSError as error:
        assert error.errno == errno.ENOSPC, error
        print("TMP_FULL", file=sys.stderr, flush=True)
        sys.exit(2)
    raise AssertionError("tmp is unbounded")
elif scenario in ("stdout", "stderr", "mixed_output"):
    chunk = b"x" * 65536
    while True:
        if scenario != "stderr":
            os.write(1, chunk)
        if scenario != "stdout":
            os.write(2, chunk)
elif scenario == "memory":
    for path in ("/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"):
        if Path(path).exists():
            assert Path(path).read_text().strip() == str(128 * 1024 * 1024)
            break
    else:
        raise AssertionError("memory controller unavailable")
    allocations = []
    while True:
        allocations.append(bytearray(1024 * 1024))
elif scenario == "cpu":
    while True:
        pass
elif scenario == "wall_sleep":
    time.sleep(60)
else:
    raise AssertionError(scenario)

print(5)
