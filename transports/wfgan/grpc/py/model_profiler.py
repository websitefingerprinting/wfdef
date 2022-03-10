import cProfile
import configparser
import io
import pstats
import sched
import time
from pstats import SortKey

from server import GenerateTraceServer

from torch.profiler import profile, record_function, ProfilerActivity
import argparse
import configparser
import logging
from concurrent.futures import ThreadPoolExecutor
from os.path import join, abspath, dirname, pardir
import numpy as np

import joblib

from gan_pb2 import GANResponse
from gan_pb2_grpc import GenerateTraceServicer, add_GenerateTraceServicer_to_server
from model import *


def main():
    BASE_DIR = abspath(join(dirname(__file__), pardir))
    CONFIG_PATH = join(BASE_DIR, 'conf.ini')
    device = torch.device('cuda:0' if torch.cuda.is_available() else 'cpu')

    cf = configparser.ConfigParser()
    cf.read(CONFIG_PATH)
    configs = cf['default']
    latent_dim = int(configs['latent_dim'])
    seq_len = int(configs['seq_len'])
    class_dim = int(configs['class_dim'])
    cell_size = int(configs['cell_size'])
    port = int(configs['gan_port_num'])
    is_bytes = cf['default'].getboolean('is_bytes')

    model_relpath = configs['model_relpath']
    scaler_relpath = configs['scaler_relpath']
    model_path = join(BASE_DIR, model_relpath)
    scaler_path = join(BASE_DIR, scaler_relpath)

    # load the generator and the scaler
    scaler = joblib.load(scaler_path)
    model = Generator(seq_len, class_dim, latent_dim).to(device)
    model.load_state_dict(torch.load(model_path, map_location=device))

    service = GenerateTraceServer(model,
                                  scaler, class_dim, is_bytes, cell_size, latent_dim, seq_len)

    def profile_one():
        cpu_time = 0  # in us
        memory_usage = 0  # in B
        with profile(activities=[ProfilerActivity.CPU],
                     profile_memory=True) as prof:
            packets = service.sample()
        for ev in prof.events():
            cpu_time += ev.cpu_time
            memory_usage += ev.cpu_memory_usage
        return cpu_time, memory_usage

    repeat = 1000000
    start_at = time.time()
    total_cpu_time = 0
    total_memory_usage = 0
    for i in range(repeat):
        cpu, mem = profile_one()
        total_cpu_time += cpu
        total_memory_usage += mem
    total_time = time.time() - start_at

    print(
        f'avg time = {total_time / repeat}, avg cpu time = {total_cpu_time / repeat}, avg memory usage = {total_memory_usage / repeat}')


# cProfile.run('main()')
main()
