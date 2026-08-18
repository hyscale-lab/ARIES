@echo off
cd "C:\Users\reman\OneDrive\Desktop\Chronos OS\aries mcp"
git checkout -b feat-mcp-client
git add .
git commit -m "feat: add in-harness MCP client adapter (Closes #35)"
echo done > flag_commit_done.txt
echo Git commit and flag creation complete.
pause
