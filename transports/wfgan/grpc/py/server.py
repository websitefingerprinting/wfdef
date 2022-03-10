import argparse
import configparser
import logging
from concurrent.futures import ThreadPoolExecutor
from os.path import join, abspath, dirname, pardir
import numpy as np

import grpc
import joblib

from gan_pb2 import GANResponse
from gan_pb2_grpc import GenerateTraceServicer, add_GenerateTraceServicer_to_server
from model import *

BASE_DIR = abspath(join(dirname(__file__), pardir))
CONFIG_PATH = join(BASE_DIR, 'conf.ini')
device = torch.device('cuda:0' if torch.cuda.is_available() else 'cpu')


class GenerateTraceServer(GenerateTraceServicer):
    def __init__(self, model, scaler, class_dim, is_bytes, cell_size, latent_dim, seq_len):
        self.model = model
        self.scaler = scaler
        self.class_dim = class_dim
        self.is_bytes = is_bytes
        self.cell_size = cell_size
        self.latent_dim = latent_dim
        self.seq_len = seq_len

    def Query(self, request, context):
        logging.info('Receive request {}'.format(request))
        packets = self.sample()
        resp = GANResponse(packets=packets)
        return resp

    def sample(self):
        n = 1
        c_ind = np.random.randint(self.class_dim)
        self.model.eval()
        with torch.no_grad():
            z = np.random.randn(n, self.latent_dim).astype('float32')
            z = torch.from_numpy(z).to(device)
            c = torch.zeros(n, self.class_dim)
            c[:, c_ind] = 1
            c = c.to(device)
            synthesized_x = self.model(z, c).cpu().numpy()
            synthesized_x = self.scaler.inverse_transform(synthesized_x).flatten()
            length = min(int(synthesized_x[0]), self.seq_len - 1)
            if length % 2 != 0:
                length -= 1
            assert length % 2 == 0
            # print(length)
            # print(synthesized_x[1:1+length].astype(int),'\n', synthesized_x[1+length:].astype(int))
            if self.is_bytes:
                synthesized_x = synthesized_x[1:1 + length].astype(int)
            else:
                # cell sequence, first round to the closest integer
                # then convert to byte sequence
                synthesized_x = np.round(synthesized_x[1:1 + length]).astype(int)
                synthesized_x *= self.cell_size
            synthesized_x[synthesized_x < self.cell_size] = self.cell_size
        return synthesized_x.tolist()


if __name__ == '__main__':
    # read args
    parser = argparse.ArgumentParser(description='GRPC server')
    parser.add_argument('-c',
                        default='default',
                        type=str,
                        help='Section id of the configuration')
    parser.add_argument('--log',
                        type=str,
                        help='Path where the log is saved.')
    args = parser.parse_args()

    # config logger
    filename = args.log
    logging.basicConfig(
        level=logging.INFO,
        format='%(asctime)s - %(levelname)s - %(message)s',
        filename=filename
    )
    logging.info(args)

    # real configs
    cf = configparser.ConfigParser()
    cf.read(CONFIG_PATH)
    configs = cf[args.c]
    latent_dim = int(configs['latent_dim'])
    seq_len = int(configs['seq_len'])
    class_dim = int(configs['class_dim'])
    cell_size = int(configs['cell_size'])
    port = int(configs['gan_port_num'])
    is_bytes = cf[args.c].getboolean('is_bytes')

    model_relpath = configs['model_relpath']
    scaler_relpath = configs['scaler_relpath']
    model_path = join(BASE_DIR, model_relpath)
    scaler_path = join(BASE_DIR, scaler_relpath)

    # load the generator and the scaler
    scaler = joblib.load(scaler_path)
    model = Generator(seq_len, class_dim, latent_dim).to(device)
    model.load_state_dict(torch.load(model_path, map_location=device))

    # # just for debugging
    # tmp_server = GenerateTraceServer(model, scaler, class_dim, is_bytes, cell_size)
    # tmp_server.sample()

    # open up grpc service
    server = grpc.server(ThreadPoolExecutor())
    add_GenerateTraceServicer_to_server(GenerateTraceServer(model,
                                                            scaler, class_dim, is_bytes, cell_size, latent_dim, seq_len), server)
    server.add_insecure_port(f'[::]:{port}')
    server.start()
    logging.info('server ready on port %r', port)
    server.wait_for_termination()