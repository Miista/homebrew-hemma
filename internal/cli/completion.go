package cli

import "fmt"

// Shell completion scripts are static (this CLI hand-rolls flag parsing, so
// there is no cobra generator). They complete the top-level verbs, the nouns
// (service/host/domain), and the known flags. Dynamic completion of existing
// service/host/domain names would require invoking the tool; keeping it static
// is intentional and sufficient. The same strings back both `hemma
// completion <shell>` (stdout) and tools/gencompletions (files for packaging),
// so the two can never drift.

// BashCompletion is the bash completion script for hemma.
const BashCompletion = `# bash completion for hemma
_hemma() {
    local cur prev words cword
    _init_completion 2>/dev/null || {
        cur="${COMP_WORDS[COMP_CWORD]}"
        prev="${COMP_WORDS[COMP_CWORD-1]}"
        cword=$COMP_CWORD
    }

    local verbs="add update remove create enable disable set list verify apply doctor measure version help completion"
    local nouns="service host domain"
    local set_keys="dns-host auth-snippet auth-service"
    local flags="--fqdn -f --host -H --backend -b --auth --auth-mode --auth-groups --all -a --fix --chdir -C --help -h"

    # First word: a verb (allow -C <dir> to precede it).
    if [[ $cword -eq 1 ]]; then
        COMPREPLY=( $(compgen -W "$verbs" -- "$cur") )
        return
    fi

    case "${COMP_WORDS[1]}" in
        add|update|remove|enable|disable)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "$nouns" -- "$cur") )
                return
            fi
            ;;
        set)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "$set_keys" -- "$cur") )
                return
            fi
            ;;
        completion)
            if [[ $cword -eq 2 ]]; then
                COMPREPLY=( $(compgen -W "bash zsh" -- "$cur") )
                return
            fi
            ;;
    esac

    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "$flags" -- "$cur") )
        return
    fi
}
complete -F _hemma hemma splitdns
`

// ZshCompletion is the zsh completion script for hemma.
const ZshCompletion = `#compdef hemma splitdns
# zsh completion for hemma

_hemma() {
    local -a verbs nouns set_keys flags
    verbs=(add update remove create enable disable set list verify apply doctor measure version help completion)
    nouns=(service host domain)
    set_keys=(dns-host auth-snippet auth-service)
    flags=(--fqdn -f --host -H --backend -b --auth --auth-mode --auth-groups --all -a --fix --chdir -C --help -h)

    if (( CURRENT == 2 )); then
        _describe 'command' verbs
        return
    fi

    case "${words[2]}" in
        add|update|remove|enable|disable)
            if (( CURRENT == 3 )); then
                _describe 'noun' nouns
                return
            fi
            ;;
        set)
            if (( CURRENT == 3 )); then
                _describe 'setting' set_keys
                return
            fi
            ;;
        completion)
            if (( CURRENT == 3 )); then
                _values 'shell' bash zsh
                return
            fi
            ;;
    esac

    _describe 'flag' flags
}

_hemma "$@"
`

// cmdCompletion prints a shell completion script to stdout. It is a hidden
// command (not listed in the verb inventory of `list`/help top matter) but does
// carry a help topic. Usage: hemma completion <bash|zsh>.
func cmdCompletion(args []string) int {
	if len(args) < 1 {
		errf("Missing the shell — expected bash or zsh.")
		hint("Usage: hemma completion <bash|zsh>")
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Print(BashCompletion)
	case "zsh":
		fmt.Print(ZshCompletion)
	default:
		errf("Unsupported shell %q — expected bash or zsh.", args[0])
		return 2
	}
	return 0
}
