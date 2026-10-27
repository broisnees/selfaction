// Real runs from github.com/broisnischal/selfaction. Durations come from the
// GitHub API (step completed_at minus started_at), not from estimates.
export const repo = 'https://github.com/broisnischal/selfaction';
export const runUrl = (id: number) => `${repo}/actions/runs/${id}`;

export type Step = { name: string; seconds: number };

/** The same push, sent to GitHub's hosted runner and to a selfaction agent. */
export const hero = {
	commit: 'a8089b6',
	hosted: {
		run: 40436118927,
		where: 'GitHub-hosted runner, ubuntu-latest',
		message: 'The job was not started because your account is locked due to a billing issue.',
	},
	local: {
		run: 40436118928,
		runner: 'macbook-pro-1-190459',
		where: 'self-hosted, macOS, ARM64',
		steps: [
			{ name: 'Set up job', seconds: 6 },
			{ name: 'actions/checkout@v4', seconds: 2 },
			{ name: 'actions/setup-go@v5', seconds: 1 },
			{ name: 'go vet ./...', seconds: 30 },
			{ name: 'go test ./...', seconds: 6 },
			{ name: 'go build', seconds: 2 },
		] satisfies Step[],
		total: 58,
	},
};

export type Run = {
	run: number;
	what: string;
	runner: string;
	machine: string;
	result: 'success' | 'failure';
	seconds: number;
	note: string;
};

/** Every run worth pointing at, newest first. */
export const runs: Run[] = [
	{
		run: 40436118928,
		what: 'local-runner on push to main',
		runner: 'macbook-pro-1-190459',
		machine: 'MacBook Pro, macOS, ARM64 (host mode)',
		result: 'success',
		seconds: 58,
		note: 'First run with per-slot HOME: setup-go 1 s, no upload of the machine’s module cache.',
	},
	{
		run: 40436118927,
		what: 'ci on push to main',
		runner: 'GitHub-hosted',
		machine: 'ubuntu-latest',
		result: 'failure',
		seconds: 3,
		note: 'Refused: the account is locked due to a billing issue. Same commit as above.',
	},
	{
		run: 40357963358,
		what: 'local-runner, dispatched to the Docker agent',
		runner: 'mac-docker-1-40fcc8',
		machine: 'Container 081ca8db3524, Linux, ARM64 (docker mode)',
		result: 'success',
		seconds: 110,
		note: 'A fresh actions-runner container, removed after the job.',
	},
	{
		run: 40356894290,
		what: 'local-runner on push',
		runner: 'macbook-pro-2-c41842',
		machine: 'MacBook Pro, macOS, ARM64 (host mode)',
		result: 'success',
		seconds: 218,
		note: 'Ran at the same time as the run below, one job per slot. 164 s went to uploading a 1.5 GB cache, the bug that led to per-slot HOME.',
	},
	{
		run: 40356880273,
		what: 'local-runner, dispatched by hand',
		runner: 'macbook-pro-1-e9e696',
		machine: 'MacBook Pro, macOS, ARM64 (host mode)',
		result: 'success',
		seconds: 197,
		note: 'The first job GitHub ever sent to a selfaction agent.',
	},
];
