"""Stem mixing desk: turn per-stem gain, mute and solo settings into a mixdown.

The heavy summing happens in the Go mixer service (mixer/main.go) so the
browser preview and the exported file are produced by the same code path.
"""
import json
import os
import shutil
import subprocess
import time
from dataclasses import dataclass, field
from typing import Dict, List, Optional

import requests

BASE_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MIXER_BIN = os.path.join(BASE_DIR, 'mixer', 'stem-mixer')
DEFAULT_MIXER_URL = 'http://127.0.0.1:8765'
STEM_NAMES = ('vocals', 'drums', 'bass', 'other')


class MixerError(RuntimeError):
    """Raised when the mixing desk cannot render a mixdown."""


@dataclass
class StemSetting:
    gain_db: float = 0.0
    mute: bool = False
    solo: bool = False


@dataclass
class MixRequest:
    stems: Dict[str, str]
    settings: Dict[str, StemSetting] = field(default_factory=dict)
    target_lufs: float = -14.0

    def normalised_settings(self) -> Dict[str, StemSetting]:
        """Fill in defaults for stems that were not touched in the UI."""
        return {name: self.settings.get(name, StemSetting()) for name in self.stems}

    def audible_stems(self) -> List[str]:
        """Solo wins over mute: if anything is soloed, only soloed stems play."""
        settings = self.normalised_settings()
        soloed = [name for name, s in settings.items() if s.solo]
        if soloed:
            return soloed
        return [name for name, s in settings.items() if not s.mute]

    def validate(self) -> None:
        if not self.stems:
            raise MixerError('No stems were given to the mixing desk.')
        for name, path in self.stems.items():
            if not os.path.exists(path):
                raise MixerError(f'Stem file for {name} is missing: {path}')
        for name, setting in self.normalised_settings().items():
            if not -60.0 <= setting.gain_db <= 12.0:
                raise MixerError(f'Gain for {name} must be between -60 and 12 dB.')
        if not self.audible_stems():
            raise MixerError('Every stem is muted, so there is nothing to render.')

    def payload(self) -> dict:
        return {
            'stems': [
                {
                    'name': name,
                    'path': os.path.abspath(path),
                    'gain_db': float(setting.gain_db),
                    'mute': bool(setting.mute),
                    'solo': bool(setting.solo),
                }
                for name, (path, setting) in (
                    (n, (p, self.normalised_settings()[n])) for n, p in self.stems.items()
                )
            ],
            'target_lufs': float(self.target_lufs),
        }


class MixerClient:
    """Talks to the Go mixdown service, starting it on demand."""

    def __init__(self, base_url: Optional[str] = None, binary: str = MIXER_BIN, autostart: bool = True):
        self.base_url = (base_url or os.environ.get('STEM_MIXER_URL') or DEFAULT_MIXER_URL).rstrip('/')
        self.binary = binary
        self.autostart = autostart
        self._process: Optional[subprocess.Popen] = None

    def is_up(self) -> bool:
        try:
            return requests.get(f'{self.base_url}/healthz', timeout=1.0).status_code == 200
        except requests.RequestException:
            return False

    def ensure_running(self, timeout: float = 10.0) -> None:
        if self.is_up():
            return
        if not self.autostart:
            raise MixerError(f'Mixer service is not reachable at {self.base_url}.')
        if not os.path.exists(self.binary):
            raise MixerError(
                f'Mixer binary not found at {self.binary}. Build it with: cd mixer && go build -o stem-mixer .'
            )
        port = self.base_url.rsplit(':', 1)[-1]
        self._process = subprocess.Popen(
            [self.binary, '-addr', f'127.0.0.1:{port}'],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.is_up():
                return
            time.sleep(0.2)
        raise MixerError('Mixer service did not start in time.')

    def render(self, request: MixRequest, output_path: str) -> str:
        request.validate()
        self.ensure_running()
        try:
            response = requests.post(
                f'{self.base_url}/mix',
                data=json.dumps(request.payload()),
                headers={'Content-Type': 'application/json'},
                timeout=120,
            )
        except requests.RequestException as exc:
            raise MixerError(f'Could not reach the mixer service: {exc}') from exc
        if response.status_code != 200:
            detail = response.text.strip() or response.reason
            raise MixerError(f'Mixer service refused the request: {detail}')
        os.makedirs(os.path.dirname(os.path.abspath(output_path)), exist_ok=True)
        with open(output_path, 'wb') as handle:
            handle.write(response.content)
        return output_path

    def close(self) -> None:
        if self._process and self._process.poll() is None:
            self._process.terminate()
            try:
                self._process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self._process.kill()
        self._process = None


_client: Optional[MixerClient] = None


def get_client() -> MixerClient:
    global _client
    if _client is None:
        _client = MixerClient()
    return _client


def collect_stems(song_dir: str) -> Dict[str, str]:
    """Find the separated stems written by the separation step in song_dir."""
    stems: Dict[str, str] = {}
    for name in STEM_NAMES:
        candidate = os.path.join(song_dir, f'{name}.wav')
        if os.path.exists(candidate):
            stems[name] = candidate
    return stems


def mix_stems(song_dir: str, settings: Optional[Dict[str, StemSetting]] = None,
              target_lufs: float = -14.0, output_name: str = 'mixdown.wav') -> str:
    """Render a mixdown of the stems in song_dir and return its path."""
    stems = collect_stems(song_dir)
    if not stems:
        raise MixerError(f'No separated stems found in {song_dir}.')
    request = MixRequest(stems=stems, settings=settings or {}, target_lufs=target_lufs)
    output_path = os.path.join(song_dir, output_name)
    return get_client().render(request, output_path)


def settings_from_form(gains: List[float], mutes: List[bool], solos: List[bool]) -> Dict[str, StemSetting]:
    """Build per-stem settings from the four rows of the mixing tab."""
    settings: Dict[str, StemSetting] = {}
    for index, name in enumerate(STEM_NAMES):
        settings[name] = StemSetting(
            gain_db=float(gains[index]) if index < len(gains) else 0.0,
            mute=bool(mutes[index]) if index < len(mutes) else False,
            solo=bool(solos[index]) if index < len(solos) else False,
        )
    return settings


def copy_preset(song_dir: str, preset_path: str) -> str:
    """Copy a saved preset next to the stems so it travels with the song."""
    if not os.path.exists(preset_path):
        raise MixerError(f'Preset not found: {preset_path}')
    target = os.path.join(song_dir, os.path.basename(preset_path))
    shutil.copyfile(preset_path, target)
    return target
