#!/bin/sh

# Get list of staged files
staged_files=$(git diff --cached --name-only)

# High-fidelity regex patterns for actual credentials:
# 1. Private key header check (PEM, RSA, PG, etc.)
PRIVATE_KEY_HEADER="-----BEGIN"

# 2. Assignment of high-entropy strings (24+ characters of Base64/Hex/Alphanumeric keys)
# This prevents blocking standard short words or generic descriptions like "token" while catching actual keys.
HIGH_ENTROPY_KEY_ASSIGNMENT="(api_key|client_secret|db_password|aws_secret|aws_access|signing_secret|private_key)\s*[:=]\s*['\"]?[a-zA-Z0-9_\-\.\/\+\=]{24,}['\"]?"

for file in $staged_files; do
    if [ -f "$file" ]; then
        # For markdown files, inspect for high-fidelity secret signatures or private keys
        if echo "$file" | grep -q '\.md$'; then
            if grep -Ei "$PRIVATE_KEY_HEADER" "$file" > /dev/null || \
               grep -Ei "$HIGH_ENTROPY_KEY_ASSIGNMENT" "$file" > /dev/null; then
                echo "=========================================================="
                echo "COMMIT BLOCKED: Actual credentials/private key found in markdown: $file"
                echo "Please scrub any real secrets before committing."
                echo "=========================================================="
                exit 1
            fi
        else
            # For source code and configuration files:
            # Check for generic secret keywords (excluding examples and this hook)
            if ! echo "$file" | grep -qE '(example|readme|pre-commit)'; then
                GENERIC_PATTERN="private_key|api_key|client_secret|db_password|aws_access_key|aws_secret|bearer"
                if grep -Ei "($GENERIC_PATTERN)" "$file" > /dev/null || \
                   grep -Ei "$PRIVATE_KEY_HEADER" "$file" > /dev/null; then
                    echo "=========================================================="
                    echo "COMMIT BLOCKED: Potential secret keyword or private key found in source: $file"
                    echo "Please verify and remove raw credentials."
                    echo "=========================================================="
                    exit 1
                fi
            fi
        fi
    fi
done

exit 0
