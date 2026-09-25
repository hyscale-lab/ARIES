"""ARIES terminal backend for Hermes.

Registers the `aries` backend, selected by TERMINAL_ENV=aries. Every command
runs through the staged ARIES gRPC client, which carries it to the task
sandbox over the per-task bridge; the client reads its target and credentials
from ARIES_GRPC_*. Nothing is synced into the sandbox: this backend never
constructs a FileSyncManager.

File tools reach the environment through BaseEnvironment.get_file_operations(),
the seam ARIES adds to the pinned Hermes before it starts.
"""

import os
import subprocess

CLIENT = "/run/aries/bin/aries-grpc"
CLIENT_ENV = ("ARIES_GRPC_TARGET", "ARIES_GRPC_IDENTITY", "ARIES_GRPC_TRUSTED")


def register(ctx):
    # Imported here so an incompatible Hermes fails this plugin with one log
    # line instead of breaking the loader.
    from agent.terminal_env_provider import TerminalEnvironmentProvider
    from tools.environments.base import BaseEnvironment
    from tools.file_operations import ShellFileOperations

    class AriesFileOperations(ShellFileOperations):
        """Typed file procedures override the shell primitives here once the
        bridge serves them; until then every call is inherited."""

    # The name must not contain ssh, docker, singularity, modal, daytona or
    # local: Hermes classifies environments by class-name substring before it
    # reads the stamped backend name.
    class AriesEnvironment(BaseEnvironment):
        def __init__(self, cwd, timeout):
            super().__init__(cwd=cwd, timeout=timeout)
            self.init_session()

        def _run_bash(self, cmd_string, *, login=False, timeout=120, stdin_data=None):
            # Hermes enforces the timeout itself by killing this process; the
            # closed connection then cancels the call on the bridge.
            argv = [CLIENT, "exec"] + (["--login"] if login else []) + ["--", cmd_string]
            proc = subprocess.Popen(
                argv,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                text=True,
                encoding="utf-8",
                errors="replace",
            )
            if stdin_data is not None:
                # The client reads all of stdin before it writes anything, so a
                # blocking write cannot deadlock against its output.
                try:
                    proc.stdin.write(stdin_data)
                    proc.stdin.close()
                except BrokenPipeError:
                    pass  # the client exited early; its exit code says why
            return proc

        def cleanup(self):
            pass  # the bridge and the sandbox belong to ARIES, not to Hermes

        def get_file_operations(self):
            return AriesFileOperations(self)

    class AriesProvider(TerminalEnvironmentProvider):
        name = "aries"

        def is_available(self):
            return os.access(CLIENT, os.X_OK) and all(os.environ.get(key) for key in CLIENT_ENV)

        def create_environment(self, *, cwd, timeout, task_id="default", image=None,
                               container_config=None, **kwargs):
            return AriesEnvironment(cwd=cwd, timeout=timeout)

    ctx.register_terminal_environment_provider(AriesProvider())
