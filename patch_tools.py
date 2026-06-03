import os
import re

tools_dir = "internal/tools"

for filename in os.listdir(tools_dir):
    if not filename.endswith(".go") or filename == "registry.go":
        continue
    filepath = os.path.join(tools_dir, filename)
    with open(filepath, "r") as f:
        content = f.read()

    # Find the struct name
    m = re.search(r"type (\w+) struct", content)
    if not m:
        continue
    struct_name = m.group(1)
    
    # Check if RiskLevel already exists
    if "RiskLevel()" in content:
        continue

    # Determine risk level
    risk = "RiskLevelHigh" if struct_name in ["ExecuteCodeTool", "WriteFileTool"] else "RiskLevelLow"
    
    # Add types import if not present
    if "github.com/wuxujun/ai-agent/internal/types" not in content and filename != "registry_test.go":
        # simple insertion
        content = re.sub(r'import \(', 'import (\n\t"github.com/wuxujun/ai-agent/internal/types"', content)

    # Add RiskLevel method after Name()
    method = f"""
func (t *{struct_name}) RiskLevel() types.RiskLevel {{
	return types.{risk}
}}
"""
    # Replace the Name method to append the new method after it
    # We find Name() and append after it.
    name_func_pattern = f"func \\(t \\*{struct_name}\\) Name\\(\\) string {{\\n\\treturn \".*?\"\\n}}"
    
    # For registry_test.go it's different, let's handle it
    if filename == "registry_test.go":
        method = f"""
func (t *{struct_name}) RiskLevel() types.RiskLevel {{
	return types.{risk}
}}
"""
        name_func_pattern = f"func \\(t \\*{struct_name}\\) Name\\(\\) string {{ return t\\.name }}"
        
    content, count = re.subn(name_func_pattern, lambda match: match.group(0) + "\n" + method, content)
    if count == 0:
        print(f"Failed to patch {filename}")
    else:
        with open(filepath, "w") as f:
            f.write(content)
        print(f"Patched {filename}")

