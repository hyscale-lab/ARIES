use codex_apply_patch::{MaybeApplyPatchVerified, maybe_parse_apply_patch_verified};
use codex_exec_server::LOCAL_FS;
use codex_utils_path_uri::PathUri;

#[tokio::main(flavor = "current_thread")]
async fn main() -> anyhow::Result<()> {
    let mut arguments = std::env::args().skip(1);
    let patch_command = arguments.next().ok_or_else(|| anyhow::anyhow!("expected the recorded patch command"))?;
    anyhow::ensure!(arguments.next().is_none(), "unexpected additional argument");
    let cwd = PathUri::parse("file:///app")?;
    let argv = ["/bin/bash".to_owned(), "-lc".to_owned(), patch_command];
    match maybe_parse_apply_patch_verified(&argv, &cwd, LOCAL_FS.as_ref(), None).await {
        MaybeApplyPatchVerified::CorrectnessError(error) => {
            let reason = error.to_string();
            println!("apply_patch verification failed: {reason}");
            anyhow::ensure!(
                reason == "invalid patch: multiple operations target /app/src/worker/worker.py",
                "recorded duplicate-target refusal did not match"
            );
        }
        _ => anyhow::bail!("the recorded command did not produce a correctness error"),
    }
    Ok(())
}
